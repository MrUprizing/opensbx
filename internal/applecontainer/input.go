package applecontainer

import (
	"errors"
	"strings"

	"opensbx/models"
)

const (
	maxInputBytes   = 64 << 10
	maxInputEntries = 1024
)

// inputBudget bounds caller-controlled argv/environment before copying them
// into CLI arguments or persistent history. Each entry includes its NUL byte;
// parts permit counting KEY=VALUE without first allocating the combined string.
type inputBudget struct {
	bytes   int
	entries int
}

func (b *inputBudget) add(parts ...string) error {
	if b.entries >= maxInputEntries || b.bytes >= maxInputBytes {
		return errors.New("guest input exceeds 64 KiB or 1024 entries")
	}
	remaining := maxInputBytes - b.bytes - 1
	for _, part := range parts {
		if len(part) > remaining {
			return errors.New("guest input exceeds 64 KiB or 1024 entries")
		}
		remaining -= len(part)
	}
	b.bytes = maxInputBytes - remaining
	b.entries++
	return nil
}

func validateCommandBudget(req models.ExecCommandRequest) error {
	var budget inputBudget
	if err := budget.add(req.Command); err != nil {
		return err
	}
	for _, arg := range req.Args {
		if err := budget.add(arg); err != nil {
			return err
		}
	}
	if req.Cwd != "" {
		if err := budget.add(req.Cwd); err != nil {
			return err
		}
	}
	for key, value := range req.Env {
		if err := budget.add(key, "=", value); err != nil {
			return err
		}
	}
	return nil
}

// Validate caller input before runtime access, persistence, or eviction of
// retained commands. Keep the budget first to bound subsequent validation work.
func validateCommandInput(req models.ExecCommandRequest) error {
	if err := validateCommandBudget(req); err != nil {
		return err
	}
	if req.Command == "" || strings.HasPrefix(req.Command, "-") || strings.ContainsRune(req.Command, 0) {
		return errors.New("invalid guest executable")
	}
	for _, arg := range req.Args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("guest argument contains NUL")
		}
	}
	if strings.ContainsRune(req.Cwd, 0) {
		return errors.New("guest working directory contains NUL")
	}
	for key, value := range req.Env {
		if !validEnv(key, value) {
			return errors.New("invalid guest environment")
		}
	}
	return nil
}
