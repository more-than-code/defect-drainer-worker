package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// Job messages (§20): the message and interrupt controls hand people's
// messages to the live run, which takes them at its next turn boundary
// (exec.Messenger, implemented by runSink).

// takeMessages answers a message or interrupt control. The images arrive as
// blobs and are fetched here, so the run writes them into its inbox.
func (w *Worker) takeMessages(ctx context.Context, j *job, f workerproto.Control) workerproto.ControlResult {
	j.mu.Lock()
	live, tok := j.live && !j.cancelled, j.token
	j.mu.Unlock()
	if !live {
		return result(f, false, nil, workerproto.CodeInternal, workerproto.DetailJobNotRunning)
	}
	msgs, err := w.fetchMessageImages(ctx, j.id, tok, f.Control.Messages)
	if err != nil {
		return result(f, false, nil, workerproto.CodeInternal, err.Error())
	}
	interrupt := f.Control.Kind == exec.ControlInterrupt
	j.mu.Lock()
	if !j.live || j.cancelled {
		j.mu.Unlock()
		return result(f, false, nil, workerproto.CodeInternal, workerproto.DetailJobNotRunning)
	}
	j.inbox = append(j.inbox, msgs...)
	var p *exec.Proc
	if interrupt && j.proc != nil && !j.proc.Group {
		// Only the implementer is interrupted: a reviewer or a verify step
		// finishes, and the messages wait for the next turn.
		j.interrupt = true
		p = j.proc
	}
	j.notifyLocked()
	j.mu.Unlock()
	if p != nil {
		go w.stopProc(p)
	}
	return result(f, true, nil, "", "")
}

// fetchMessageImages downloads each blob image of msgs.
func (w *Worker) fetchMessageImages(ctx context.Context, jobID, tok string, msgs []exec.InboxMessage) ([]exec.InboxMessage, error) {
	out := make([]exec.InboxMessage, 0, len(msgs))
	for _, m := range msgs {
		imgs := make([]exec.MessageImage, 0, len(m.Images))
		for _, img := range m.Images {
			if img.SHA256 == "" {
				imgs = append(imgs, img)
				continue
			}
			data, err := w.getBlob(ctx, jobID, img.SHA256, tok)
			if err != nil {
				return nil, fmt.Errorf("message %s image: %v", m.ID, err)
			}
			if hashHex(data) != img.SHA256 || len(data) > exec.MaxMessageImage {
				return nil, fmt.Errorf("message %s image: blob does not match", m.ID)
			}
			img.Data, img.SHA256 = data, ""
			imgs = append(imgs, img)
		}
		m.Images = imgs
		out = append(out, m)
	}
	return out, nil
}

// notifyLocked wakes a run waiting for messages.
func (j *job) notifyLocked() {
	select {
	case j.msgSig <- struct{}{}:
	default:
	}
}

// --- exec.Messenger ---

func (s runSink) TakeMessages() []exec.InboxMessage {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	if s.gen != s.j.gen {
		return nil
	}
	out := s.j.inbox
	s.j.inbox = nil
	return out
}

func (s runSink) TakeInterrupt() bool {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	v := s.j.interrupt
	s.j.interrupt = false
	return v && s.gen == s.j.gen
}

func (s runSink) WaitMessages(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		s.j.mu.Lock()
		if s.gen != s.j.gen || s.j.cancelled {
			s.j.mu.Unlock()
			return false
		}
		if len(s.j.inbox) > 0 {
			s.j.mu.Unlock()
			return true
		}
		sig, done := s.j.msgSig, s.j.ctx.Done()
		s.j.mu.Unlock()
		select {
		case <-sig:
		case <-done:
			return false
		case <-t.C:
			return false
		}
	}
}
