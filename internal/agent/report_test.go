package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/elprovo/elprovo/protocol"
)

func TestSecretWithLineBreaksIsHiddenOnEveryLine(t *testing.T) {
	var sent []protocol.JobLog
	rep := newReporter(func(_ context.Context, _ string, payload any) error {
		if l, ok := payload.(protocol.JobLog); ok {
			sent = append(sent, l)
		}
		return nil
	}, protocol.Job{ID: "op1", LeaseToken: "lt1"}, slog.New(slog.DiscardHandler))
	rep.hideValues("first-line-secret\r\nsecond-line-secret")

	w := rep.writer("start", "stdout")
	_, _ = w.Write([]byte("key: first-line-secret\rsecond-line-secret done\n"))
	_ = w.Close()
	rep.flush(context.Background())

	if len(sent) != 1 || len(sent[0].Lines) != 2 || sent[0].Lines[0].Text != "key: [secret]" || sent[0].Lines[1].Text != "[secret] done" {
		t.Fatalf("sent = %+v, want both lines with the secret hidden", sent)
	}
}
