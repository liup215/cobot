package transcript

// queue.go — the write-behind FIFO. Push and PushUsage share one channel so
// a turn's usage stamp is ALWAYS applied after that turn's messages have
// been appended (the ordering that makes StampUsage's "latest assistant row"
// target correct). Blocking sends give backpressure instead of reordering;
// Flush() makes the pipeline deterministic for tests and shutdown.

import (
	"fmt"
	"log/slog"
	"time"

	cobottypes "github.com/liup215/cobot/types"
)

type usageFact struct{ in, out int }

type task struct {
	sessionID string
	msgs      []cobottypes.Message
	usage     *usageFact
	quit      bool
	barrier   chan struct{}
}

const (
	flushInterval = 500 * time.Millisecond
	flushBatch    = 50
)

// Push queues messages for asynchronous append. Ordering with earlier Push /
// PushUsage calls is preserved; the call blocks when the queue is full
// (backpressure — dropping or applying out of band would reorder the
// transcript).
func (s *Store) Push(sessionID string, msgs []cobottypes.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	return s.enqueue(task{sessionID: sessionID, msgs: msgs})
}

// PushUsage queues a turn's usage fact to stamp after the queued messages
// land. Zero measurements are skipped — the estimate stays honest.
func (s *Store) PushUsage(sessionID string, input, output int) {
	if sessionID == "" || (input <= 0 && output <= 0) {
		return
	}
	_ = s.enqueue(task{sessionID: sessionID, usage: &usageFact{in: input, out: output}})
}

func (s *Store) enqueue(t task) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return fmt.Errorf("transcript: store closed")
	}
	s.tasks <- t // blocks when full; the worker keeps draining
	return nil
}

// Flush blocks until every task queued before the call has been applied.
func (s *Store) Flush() error {
	barrier := make(chan struct{})
	if err := s.enqueue(task{barrier: barrier}); err != nil {
		return err
	}
	<-barrier
	return nil
}

// Close stops accepting work, drains the queue and closes the database.
// Safe to call multiple times.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.tasks <- task{quit: true} // under the lock: no enqueue can follow
		s.mu.Unlock()
		<-s.done
		s.db.Close()
	})
	return nil
}

// worker merges consecutive same-session pushes into one append (batch
// efficiency), applies usage stamps after their messages, and flushes on the
// interval.
func (s *Store) worker() {
	defer close(s.done)

	type segment struct {
		sessionID string
		msgs      []cobottypes.Message
	}
	var pending []segment
	pendingMsgs := 0
	flushPending := func() {
		for _, seg := range pending {
			if err := s.Append(seg.sessionID, seg.msgs); err != nil {
				slog.Warn("transcript: append failed", "session", seg.sessionID, "err", err)
			}
		}
		pending = pending[:0]
		pendingMsgs = 0
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case t := <-s.tasks:
			if t.quit {
				flushPending()
				return
			}
			if t.barrier != nil {
				flushPending()
				close(t.barrier)
				continue
			}
			if t.usage != nil {
				// The turn's messages must exist before the stamp targets
				// their assistant row — apply everything queued first.
				flushPending()
				if err := s.StampUsage(t.sessionID, t.usage.in, t.usage.out); err != nil {
					slog.Warn("transcript: stamp usage failed", "session", t.sessionID, "err", err)
				}
				continue
			}
			if n := len(pending); n > 0 && pending[n-1].sessionID == t.sessionID {
				pending[n-1].msgs = append(pending[n-1].msgs, t.msgs...)
			} else {
				pending = append(pending, segment{sessionID: t.sessionID, msgs: t.msgs})
			}
			pendingMsgs += len(t.msgs)
			if pendingMsgs >= flushBatch {
				flushPending()
			}
		case <-ticker.C:
			flushPending()
		}
	}
}
