package store

import (
	"context"
	"errors"
	"testing"
)

func TestHookRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	channel, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.CreatePrincipal(ctx, PrincipalAgent, "monitor")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		token     string
		wantError error
	}{
		{name: "existing", token: "test-token"},
		{name: "unknown", token: "missing", wantError: ErrNotFound},
	}
	if _, err := s.CreateHook(ctx, "test-token", channel.ID, principal.ID); err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook, err := s.HookByToken(ctx, tt.token)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("HookByToken error = %v, want %v", err, tt.wantError)
			}
			if tt.wantError == nil && (hook.Token != tt.token || hook.ChannelID != channel.ID ||
				hook.PrincipalID != principal.ID || hook.CreatedAt.IsZero()) {
				t.Errorf("HookByToken = %+v", hook)
			}
		})
	}
}
