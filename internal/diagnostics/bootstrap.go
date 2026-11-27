package diagnostics

import (
	"context"
	"log/slog"
)

// Bootstrap buffers only bounded, payload-free setup records, then replays them
// using validated options. Invalid options deterministically fall back to text.
type Bootstrap struct{ records []slog.Record }

func (b *Bootstrap) Enabled(context.Context, slog.Level) bool { return true }
func (b *Bootstrap) Handle(_ context.Context, r slog.Record) error {
	if len(b.records) < 64 {
		b.records = append(b.records, r.Clone())
	}
	return nil
}
func (b *Bootstrap) WithAttrs([]slog.Attr) slog.Handler { return b }
func (b *Bootstrap) WithGroup(string) slog.Handler      { return b }
func (b *Bootstrap) Replay(ctx context.Context, log *slog.Logger) {
	for _, r := range b.records {
		if log.Enabled(ctx, r.Level) {
			_ = log.Handler().Handle(ctx, r)
		}
	}
	b.records = nil
}
