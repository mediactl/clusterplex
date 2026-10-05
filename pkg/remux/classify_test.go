package remux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	pb "github.com/mediactl/clusterplex/proto"
)

type players map[string]string

func (p players) PlayerProduct(_ context.Context, id string) (string, error) {
	if v, ok := p[id]; ok {
		return v, nil
	}
	return "", errors.New("unreachable")
}

type slow struct{}

func (slow) PlayerProduct(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func arcaneRequest() *pb.ExecRequest {
	args, env := loggedJob(arcane)
	return &pb.ExecRequest{TargetBinary: "Plex Transcoder", Args: args, Env: env}
}

func TestOnlyPlexWebsRemuxIsRouted(t *testing.T) {
	ctx := t.Context()
	_, ok := Classify(ctx, arcaneRequest(), players{"f168tc7vl2desp78pov5ktwu": "Plex Web"})
	assert.True(t, ok)
	_, ok = Classify(ctx, arcaneRequest(), players{"f168tc7vl2desp78pov5ktwu": "Plex for Android (TV)"})
	assert.False(t, ok, "another client's DASH job stays Plex's")
	_, ok = Classify(ctx, arcaneRequest(), players{})
	assert.False(t, ok, "a session Plex cannot name stays Plex's")
	req := arcaneRequest()
	req.TargetBinary = "Plex Media Scanner"
	_, ok = Classify(ctx, req, players{"f168tc7vl2desp78pov5ktwu": "Plex Web"})
	assert.False(t, ok)
}

func TestASlowSessionLookupFallsBackToPlex(t *testing.T) {
	start := time.Now()
	_, ok := Classify(t.Context(), arcaneRequest(), slow{})
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 3*time.Second)
}
