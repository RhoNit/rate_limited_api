// Package queue implements a RabbitMQ-backed retry queue for rate-limited
// requests.
//
// Topology (TTL + Dead Letter Exchange pattern)
//
//	Publisher → ratelimit.waiting  (per-message TTL = retry_after)
//	                 │ (message expires)
//	                 ▼  DLX → default exchange → ratelimit.retry  → Consumer
//
// Flow:
//  1. A rate-limited HTTP request is published to ratelimit.waiting with
//     Expiration = retry_after milliseconds.
//  2. When the TTL fires, RabbitMQ moves the message to ratelimit.retry
//     via the dead-letter exchange (DLX).
//  3. The consumer calls limiter.CheckAndRecord. If the window has
//     opened, the request is processed (acked). If somehow the window is
//     still full (clock skew, burst), the message is re-queued with
//     exponential backoff up to MaxAttempts.
//
// Reconnect: the consumer goroutine monitors the AMQP connection via
// NotifyClose and transparently reconnects with exponential backoff.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/RhoNit/rate-limited-api/internal/limiter"
)

const (
	waitingQueue = "ratelimit.waiting"
	retryQueue   = "ratelimit.retry"
	// MaxAttempts is the total number of delivery attempts before a job is
	// dropped (includes the first consumer attempt).
	MaxAttempts = 3
)

// Job is the message payload stored in RabbitMQ.
type Job struct {
	JobID    string          `json:"job_id"`
	UserID   string          `json:"user_id"`
	Payload  json.RawMessage `json:"payload"`
	Attempts int             `json:"attempts"` // incremented on each retry
}

// Stats holds in-memory counters (reset on process restart).
type Stats struct {
	Queued    int64 `json:"queued"`
	Processed int64 `json:"processed"`
	Dropped   int64 `json:"dropped"`
}

// Manager owns the AMQP connection, publishes rate-limited jobs, and
// runs the consumer goroutine.
type Manager struct {
	amqpURL string
	rl      limiter.Limiter
	log     *zap.Logger

	mu        sync.Mutex
	ch        *amqp.Channel // guarded by mu; nil when disconnected
	closeOnce sync.Once
	done      chan struct{}

	queued    atomic.Int64
	processed atomic.Int64
	dropped   atomic.Int64
}

// NewManager creates a Manager but does NOT dial yet (call Start).
func NewManager(amqpURL string, rl limiter.Limiter, log *zap.Logger) *Manager {
	return &Manager{
		amqpURL: amqpURL,
		rl:      rl,
		log:     log.Named("queue"),
		done:    make(chan struct{}),
	}
}

// Start dials RabbitMQ, declares the topology, and launches the consumer
// in a background goroutine. Returns an error if the initial connection
// fails (fail-fast at startup).
func (m *Manager) Start(ctx context.Context) error {
	conn, ch, err := m.dial()
	if err != nil {
		return err
	}
	m.setChannel(ch)
	go m.consumeLoop(ctx, conn, ch)
	m.log.Info("connected", zap.String("url", m.amqpURL))
	return nil
}

// Publish enqueues a rate-limited job. retryAfter tells RabbitMQ how long
// to hold the message before delivering it to the retry queue.
func (m *Manager) Publish(ctx context.Context, job Job, retryAfter time.Duration) error {
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	ttlMs := retryAfter.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 100
	}

	m.mu.Lock()
	ch := m.ch
	m.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("queue: not connected")
	}

	if err := ch.PublishWithContext(ctx, "", waitingQueue, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Expiration:   fmt.Sprintf("%d", ttlMs),
			Body:         body,
		}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	m.queued.Add(1)
	m.log.Info("job queued",
		zap.String("job_id", job.JobID),
		zap.String("user_id", job.UserID),
		zap.Duration("retry_after", retryAfter),
	)
	return nil
}

// Stats returns a snapshot of queue counters.
func (m *Manager) Stats() Stats {
	return Stats{
		Queued:    m.queued.Load(),
		Processed: m.processed.Load(),
		Dropped:   m.dropped.Load(),
	}
}

// Close signals the consumer to stop and closes the AMQP channel.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() { close(m.done) })
	m.mu.Lock()
	ch := m.ch
	m.mu.Unlock()
	if ch != nil {
		return ch.Close()
	}
	return nil
}

// NewJobID generates a short random hex ID for a job.
func NewJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- internal -----------------------------------------------------------------

func (m *Manager) dial() (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(m.amqpURL)
	if err != nil {
		return nil, nil, fmt.Errorf("amqp dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("open channel: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("declare topology: %w", err)
	}
	return conn, ch, nil
}

func declareTopology(ch *amqp.Channel) error {
	if _, err := ch.QueueDeclare(retryQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare retry queue: %w", err)
	}
	args := amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": retryQueue,
	}
	if _, err := ch.QueueDeclare(waitingQueue, true, false, false, false, args); err != nil {
		return fmt.Errorf("declare waiting queue: %w", err)
	}
	return nil
}

func (m *Manager) setChannel(ch *amqp.Channel) {
	m.mu.Lock()
	m.ch = ch
	m.mu.Unlock()
}

func (m *Manager) consumeLoop(ctx context.Context, conn *amqp.Connection, ch *amqp.Channel) {
	defer conn.Close()

	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))

	deliveries, err := ch.Consume(retryQueue, "", false, false, false, false, nil)
	if err != nil {
		m.log.Error("consume register failed, will reconnect", zap.Error(err))
		go m.reconnectLoop(ctx)
		return
	}

	for {
		select {
		case <-m.done:
			return
		case <-ctx.Done():
			return
		case amqpErr, ok := <-connClosed:
			m.log.Warn("connection lost, reconnecting",
				zap.Bool("graceful", !ok),
				zap.Any("reason", amqpErr),
			)
			m.setChannel(nil)
			go m.reconnectLoop(ctx)
			return
		case d, ok := <-deliveries:
			if !ok {
				return
			}
			m.process(ctx, d)
		}
	}
}

func (m *Manager) reconnectLoop(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-m.done:
			return
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		conn, ch, err := m.dial()
		if err != nil {
			m.log.Warn("reconnect failed",
				zap.Error(err),
				zap.Duration("next_retry", backoff),
			)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		m.setChannel(ch)
		m.log.Info("reconnected")
		m.consumeLoop(ctx, conn, ch)
		return
	}
}

func (m *Manager) process(ctx context.Context, d amqp.Delivery) {
	var job Job
	if err := json.Unmarshal(d.Body, &job); err != nil {
		m.log.Error("unmarshal failed, dropping message", zap.Error(err))
		d.Ack(false)
		return
	}

	res, err := m.rl.CheckAndRecord(ctx, job.UserID)
	if err != nil {
		m.log.Error("limiter error, nacking",
			zap.String("job_id", job.JobID),
			zap.String("user_id", job.UserID),
			zap.Error(err),
		)
		d.Nack(false, true)
		return
	}

	if res.Allowed {
		m.log.Info("job processed",
			zap.String("job_id", job.JobID),
			zap.String("user_id", job.UserID),
			zap.Int("attempt", job.Attempts+1),
		)
		m.processed.Add(1)
		d.Ack(false)
		return
	}

	// Still rate-limited — retry with exponential backoff if budget remains.
	job.Attempts++
	if job.Attempts >= MaxAttempts {
		m.log.Warn("job dropped after max attempts",
			zap.String("job_id", job.JobID),
			zap.String("user_id", job.UserID),
			zap.Int("attempts", job.Attempts),
		)
		m.dropped.Add(1)
		d.Ack(false)
		return
	}

	backoff := res.RetryAfter * time.Duration(1<<uint(job.Attempts))
	if err := m.Publish(ctx, job, backoff); err != nil {
		m.log.Error("re-publish failed, nacking",
			zap.String("job_id", job.JobID),
			zap.Error(err),
		)
		d.Nack(false, true)
		return
	}
	m.log.Info("job re-queued",
		zap.String("job_id", job.JobID),
		zap.String("user_id", job.UserID),
		zap.Int("attempt", job.Attempts),
		zap.Duration("retry_after", backoff),
	)
	d.Ack(false)
}
