package main

import (
	"context"
	"log/slog"
)

// Runtime diagnostics may remain plaintext. Free-form fields (commands, input,
// output, paths, host names and error strings) must not bypass encrypted history
// via debug logs. The handler sits outside every component logger in main.
type diagnosticHandler struct{ next slog.Handler }

func (h diagnosticHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func diagnosticAttribute(attr slog.Attr) (slog.Attr, bool) {
	if attr.Key == "err" || attr.Key == "error" {
		return slog.Bool("error_present", attr.Value.Any() != nil), true
	}
	switch attr.Value.Kind() {
	case slog.KindBool, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration:
		return attr, true
	case slog.KindString:
		switch attr.Key {
		case "tool", "version", "stage", "reason", "error_code":
			value := attr.Value.String()
			if len(value) > 80 {
				return slog.Attr{}, false
			}
			for _, b := range []byte(value) {
				if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.' || b == '+' || b == ' ') {
					return slog.Attr{}, false
				}
			}
			return attr, true
		}
	}
	return slog.Attr{}, false
}

func (h diagnosticHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		if value, ok := diagnosticAttribute(attr); ok {
			clean.AddAttrs(value)
		}
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h diagnosticHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var clean []slog.Attr
	for _, attr := range attrs {
		if value, ok := diagnosticAttribute(attr); ok {
			clean = append(clean, value)
		}
	}
	return diagnosticHandler{h.next.WithAttrs(clean)}
}

func (h diagnosticHandler) WithGroup(string) slog.Handler { return h }
