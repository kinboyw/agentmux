package runutil

import (
	"context"
	"os/exec"
	"time"
)

func CommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	out, err := command.CombinedOutput()
	return string(out), err
}

func ShortContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}
