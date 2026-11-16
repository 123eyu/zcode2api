package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"zcode2api/internal/server/shared"
)

// Explicit paths are relative to the project root; bare names use PATH first.
func ResolveExecutable(root, binary string, fallbacks ...string) (string, error) {
	if !filepath.IsAbs(binary) && strings.ContainsAny(binary, "/\\") {
		binary = filepath.Join(root, binary)
	}
	resolved, err := exec.LookPath(binary)
	for _, candidate := range fallbacks {
		if err == nil {
			break
		}
		resolved, err = exec.LookPath(candidate)
	}
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func NewJSONCommand(ctx context.Context, binary string, input shared.M, args ...string) (*exec.Cmd, *limitedProcessOutput) {
	command := exec.CommandContext(ctx, binary, args...)
	HideChildWindow(command)
	command.Stdin = strings.NewReader(shared.JSON(input))
	output := &limitedProcessOutput{}
	command.Stdout, command.Stderr = output, io.Discard
	command.WaitDelay = time.Second
	return command, output
}

// Bound stdout from the Node.js and Python helpers before decoding their JSON.
type limitedProcessOutput struct {
	Value []byte `json:"-"`
}

func (w *limitedProcessOutput) Write(p []byte) (int, error) {
	if len(w.Value)+len(p) > 64<<10 {
		return 0, errors.New("process output exceeds 64 KiB")
	}
	w.Value = append(w.Value, p...)
	return len(p), nil
}
