package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// OpenCodeLoginTarget identifies one of the OpenCode subscriptions that the
// Escape login menu can configure.
var ErrOpenCodeCLIMissing = errors.New("opencode CLI is not installed")

type OpenCodeLoginTarget string

const (
	OpenCodeLoginGo  OpenCodeLoginTarget = "opencode-go"
	OpenCodeLoginZen OpenCodeLoginTarget = "opencode-zen"
)

// ProviderID returns the provider ID understood by the OpenCode CLI.
func (t OpenCodeLoginTarget) ProviderID() string {
	if t == OpenCodeLoginZen {
		return OpenCodeZenProviderID
	}
	return "opencode-go"
}

// LoginOpenCode delegates authentication to the installed OpenCode CLI. The
// CLI owns the provider-specific browser flow and writes its auth store; this
// function only verifies the resulting Go credential.
func LoginOpenCode(ctx context.Context, target OpenCodeLoginTarget, input io.Reader, output, errOutput io.Writer) error {
	var providerID string
	switch target {
	case OpenCodeLoginGo:
		providerID = "opencode-go"
		if OpenCodeGoKey() != "" {
			return nil
		}
	case OpenCodeLoginZen:
		providerID = OpenCodeZenProviderID
		if OpenCodeZenKey() != "" {
			return nil
		}
	default:
		return fmt.Errorf("unknown OpenCode login target %q", target)
	}

	binary, err := exec.LookPath("opencode")
	if err != nil {
		return fmt.Errorf("%w; install it or set the provider API key: %v", ErrOpenCodeCLIMissing, err)
	}
	cmd := exec.CommandContext(ctx, binary, "auth", "login", "--provider", providerID)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = errOutput
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("opencode login for %s failed: %w", target, err)
	}
	if target == OpenCodeLoginGo && OpenCodeGoKey() == "" {
		return fmt.Errorf("opencode login completed without an OpenCode Go credential")
	}
	return nil
}
