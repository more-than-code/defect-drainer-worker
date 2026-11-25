package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dexec "github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// §21: an interactive job asks, waits, gets the answer through a message
// control at the turn boundary, and finishes. The agent's messages come
// back as message events, their images as blobs.
func TestInteractiveJobAsksAndGetsAnAnswer(t *testing.T) {
	fixture := claudeFixture(t, "c1-basic")
	agent := "n=$(cat .iter 2>/dev/null || echo 0); n=$((n+1)); echo $n > .iter\n" +
		"if [ $n = 1 ]; then\n" +
		"  grep -q 'This job is interactive' MESSAGES.md || exit 4\n" +
		"  printf '{\"text\":\"Which button do you mean?\",\"ask\":true}\\n' >> OUTBOX.jsonl\n" +
		"  cat '" + fixture + "'; exit 0\nfi\n" +
		"grep -q 'the blue one' INBOX.md || exit 3\n" +
		"grep -q 'answered' \"$HOME/../argv.log\" 2>/dev/null; \n" +
		fixArtifacts(testDefect) +
		"printf '{\"text\":\"Fixed the blue button.\",\"images\":[\"fix-evidence/" + testDefect + "/fix-01.png\"],\"links\":[{\"url\":\"javascript:alert(1)\"}]}\\n' >> OUTBOX.jsonl\n" +
		"cat '" + fixture + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_ask1"
	spec := fixSpec(id)
	spec.Caps.AgentMaxIters = 3
	spec.Messaging = &dexec.MessagingSpec{Autonomy: dexec.AutonomyInteractive, AskTimeoutSec: 60}
	h.offerJob(spec, nil)

	var ask *dexec.AgentMessage
	eventually(t, 60*time.Second, "the agent's question", func() bool {
		for _, e := range h.fs.jobEvents(id) {
			if e.Message != nil && e.Message.Ask {
				ask = e.Message
			}
		}
		return ask != nil
	})
	if ask.Text != "Which button do you mean?" {
		t.Fatalf("ask = %+v", ask)
	}
	eventually(t, 30*time.Second, "waiting event", func() bool {
		for _, e := range h.fs.jobEvents(id) {
			if e.Waiting != nil && e.Waiting.Until != "" {
				return true
			}
		}
		return false
	})
	r := h.controlAndWait(id, "rq_msg1", dexec.Control{Kind: dexec.ControlMessage, Messages: []dexec.InboxMessage{
		{ID: "jmsg_1", Author: "Joe", Text: "the blue one, on the totals page"},
	}}, 1)
	if !r.OK {
		t.Fatalf("message control %+v", r)
	}
	d := h.waitDone(id, 60*time.Second)
	events := h.fs.jobEvents(id)
	if d.State != "completed" {
		t.Fatalf("done = %+v\n%s", d, joinLines(events))
	}
	var delivered, answered bool
	var final *dexec.AgentMessage
	for _, e := range events {
		if e.Delivered != nil && len(e.Delivered.IDs) == 1 && e.Delivered.IDs[0] == "jmsg_1" {
			delivered = true
		}
		if e.Waiting != nil && e.Waiting.Answered {
			answered = true
		}
		if e.Message != nil && !e.Message.Ask {
			final = e.Message
		}
	}
	if !delivered || !answered {
		t.Fatalf("delivered=%v answered=%v\n%s", delivered, answered, joinLines(events))
	}
	if final == nil || final.Text != "Fixed the blue button." || len(final.Images) != 1 || final.Images[0].Data != nil || !workerproto.ValidSHA256(final.Images[0].SHA256) {
		t.Fatalf("final message %+v", final)
	}
	if len(final.Links) != 0 || len(final.Dropped) != 1 {
		t.Fatalf("a javascript: link must be dropped: %+v", final)
	}
	inbox, err := os.ReadFile(filepath.Join(h.paths.Handoff(id), "INBOX.md"))
	if err != nil || !strings.Contains(string(inbox), "> the blue one") || !strings.Contains(string(inbox), "from Joe") {
		t.Fatalf("INBOX.md: %v\n%s", err, inbox)
	}
	argv, _ := os.ReadFile(h.argv)
	if !strings.Contains(string(argv), "Your question was answered") {
		t.Fatalf("iteration 2 did not get the answer prompt:\n%s", argv)
	}
	// A message to a job with no live run is refused, for the server to
	// answer it with a chat run instead.
	r = h.controlAndWait(id, "rq_msg2", dexec.Control{Kind: dexec.ControlMessage, Messages: []dexec.InboxMessage{{ID: "jmsg_2", Text: "thanks"}}}, 1)
	if r.OK || r.Detail != workerproto.DetailJobNotRunning {
		t.Fatalf("message to a finished job %+v", r)
	}
}

// §21: an unanswered question times out and the agent is told to go on.
func TestInteractiveQuestionTimesOut(t *testing.T) {
	fixture := claudeFixture(t, "c1-basic")
	agent := "n=$(cat .iter 2>/dev/null || echo 0); n=$((n+1)); echo $n > .iter\n" +
		"if [ $n = 1 ]; then printf '{\"text\":\"Which one?\",\"ask\":true}\\n' >> OUTBOX.jsonl; cat '" + fixture + "'; exit 0; fi\n" +
		fixArtifacts(testDefect) + "cat '" + fixture + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_ask2"
	spec := fixSpec(id)
	spec.Caps.AgentMaxIters = 3
	spec.Messaging = &dexec.MessagingSpec{Autonomy: dexec.AutonomyInteractive, AskTimeoutSec: 1}
	h.offerJob(spec, nil)
	d := h.waitDone(id, 60*time.Second)
	events := h.fs.jobEvents(id)
	if d.State != "completed" {
		t.Fatalf("done = %+v\n%s", d, joinLines(events))
	}
	timedOut := false
	for _, e := range events {
		if e.Waiting != nil && e.Waiting.TimedOut {
			timedOut = true
		}
	}
	argv, _ := os.ReadFile(h.argv)
	if !timedOut || !strings.Contains(string(argv), "No answer came") {
		t.Fatalf("timedOut=%v argv:\n%s", timedOut, argv)
	}
}

// §21: an autonomous job never waits: an ask is a plain message.
func TestAutonomousJobDoesNotWait(t *testing.T) {
	fixture := claudeFixture(t, "c1-basic")
	agent := "grep -q 'This job is autonomous' MESSAGES.md || exit 4\n" +
		"printf '{\"text\":\"Which one?\",\"ask\":true}\\n' >> OUTBOX.jsonl\n" +
		fixArtifacts(testDefect) + "cat '" + fixture + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_auto1"
	spec := fixSpec(id)
	spec.Messaging = &dexec.MessagingSpec{}
	h.offerJob(spec, nil)
	d := h.waitDone(id, 60*time.Second)
	events := h.fs.jobEvents(id)
	if d.State != "completed" {
		t.Fatalf("done = %+v\n%s", d, joinLines(events))
	}
	for _, e := range events {
		if e.Waiting != nil {
			t.Fatalf("an autonomous job waited:\n%s", joinLines(events))
		}
	}
}

// §21: a chat run on a finished job resumes the session with no shell and
// no edits, answers, and reports a worktree it changed.
func TestChatRunAnswersReadOnly(t *testing.T) {
	fixture := claudeFixture(t, "c1-basic")
	agent := "case \"$*\" in *'--disallowedTools Bash,Edit'*)\n" +
		"  grep -q 'why the rounding' INBOX.md || exit 5\n" +
		"  cat '" + fixture + "'; exit 0;;\nesac\n" +
		fixArtifacts(testDefect) + "cat '" + fixture + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_chat1"
	spec := fixSpec(id)
	fj := h.offerJob(spec, nil)
	h.waitDone(id, 60*time.Second)
	var session string
	for _, e := range h.fs.jobEvents(id) {
		if e.AgentRun != nil && e.AgentRun.SessionID != "" {
			session = e.AgentRun.SessionID
		}
	}
	if session == "" {
		t.Fatal("no session id recorded")
	}
	chat := spec
	chat.Mode = dexec.ModeChat
	chat.PriorAgentRun = &dexec.AgentRun{SessionID: session}
	chat.Chat = &dexec.ChatSpec{Messages: []dexec.InboxMessage{{ID: "jmsg_c1", Author: "Joe", Text: "why the rounding change?"}}, MaxTurns: 5}
	h.fs.mu.Lock()
	fj.spec = chat
	h.fs.mu.Unlock()
	before := len(h.fs.jobEvents(id))
	h.fs.lease(id)
	r := h.controlAndWait(id, "rq_chat1", dexec.Control{Kind: dexec.ControlResume, SpecURL: workerproto.PathPrefix + "jobs/" + id + "/spec"}, 1)
	if !r.OK {
		t.Fatalf("resume for chat %+v", r)
	}
	var cr *dexec.ChatRun
	var answer *dexec.AgentMessage
	eventually(t, 60*time.Second, "chat run", func() bool {
		evs := h.fs.jobEvents(id)
		for _, e := range evs[before:] {
			if e.ChatRun != nil {
				cr = e.ChatRun
			}
			if e.Message != nil && e.Message.Chat {
				answer = e.Message
			}
		}
		return cr != nil || (len(evs) > before+1 && evs[len(evs)-1].Kind == dexec.EventDone)
	})
	if cr == nil {
		t.Fatalf("no chat run:\n%s", joinLines(h.fs.jobEvents(id)[before:]))
	}
	if cr.Error != "" || answer == nil || answer.Text == "" || len(answer.ReplyTo) != 1 || answer.ReplyTo[0] != "jmsg_c1" {
		t.Fatalf("chat run %+v answer %+v\n%s", cr, answer, joinLines(h.fs.jobEvents(id)))
	}
	if cr.Changed {
		t.Fatal("a read-only chat run reported changed worktrees")
	}
	argv, _ := os.ReadFile(h.argv)
	if !strings.Contains(string(argv), "--resume "+session) {
		t.Fatalf("chat run did not resume the session:\n%s", argv)
	}
}

// §21: Send now stops the agent mid-step; the next iteration takes the
// message, and the stopped iteration is not a failure.
func TestInterruptDeliversAtOnce(t *testing.T) {
	fixture := claudeFixture(t, "c1-basic")
	agent := "n=$(cat .iter 2>/dev/null || echo 0); n=$((n+1)); echo $n > .iter\n" +
		"if [ $n = 1 ]; then cat '" + fixture + "'; touch started; sleep 30; exit 0; fi\n" +
		"grep -q 'stop that' INBOX.md || exit 3\n" +
		fixArtifacts(testDefect) + "cat '" + fixture + "'\nexit 0\n"
	h := newHarness(t, harnessOpts{agent: agent})
	id := "bjob_int1"
	spec := fixSpec(id)
	spec.Caps.AgentMaxIters = 3
	spec.Messaging = &dexec.MessagingSpec{}
	h.offerJob(spec, nil)
	eventually(t, 30*time.Second, "agent started", func() bool {
		_, err := os.Stat(filepath.Join(h.paths.Handoff(id), "started"))
		return err == nil
	})
	t0 := time.Now()
	r := h.controlAndWait(id, "rq_int1", dexec.Control{Kind: dexec.ControlInterrupt, Messages: []dexec.InboxMessage{{ID: "jmsg_i1", Text: "stop that, wrong file"}}}, 1)
	if !r.OK {
		t.Fatalf("interrupt %+v", r)
	}
	d := h.waitDone(id, 60*time.Second)
	events := h.fs.jobEvents(id)
	if d.State != "completed" || time.Since(t0) > 20*time.Second {
		t.Fatalf("done = %+v after %s\n%s", d, time.Since(t0), joinLines(events))
	}
	interrupted := false
	for _, e := range events {
		if e.Delivered != nil && e.Delivered.Interrupted {
			interrupted = true
		}
	}
	argv, _ := os.ReadFile(h.argv)
	if !interrupted || !strings.Contains(string(argv), "stopped you mid-step") {
		t.Fatalf("interrupted=%v argv:\n%s", interrupted, argv)
	}
}
