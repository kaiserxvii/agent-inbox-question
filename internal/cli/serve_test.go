package cli

import (
	"context"
	"testing"
)

func TestRunServeAcceptsCommandSpecificFlagsAfterCommand(t *testing.T) {
	app, err := NewApp(t.TempDir())
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = app.RunServe(ctx, []string{
		"--reset-interval", "25ms",
		"--scan-interval", "10ms",
	})
	if err != nil {
		t.Fatalf("RunServe: %v", err)
	}
}
