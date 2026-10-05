package remux

import (
	"context"
	"time"

	pb "github.com/mediactl/clusterplex/proto"
)

// PlexWeb is the product Plex's web player names itself.
const PlexWeb = "Plex Web"

// lookupTimeout bounds asking Plex who is playing; past it the job is Plex's.
const lookupTimeout = 2 * time.Second

type Players interface {
	PlayerProduct(ctx context.Context, transcodeSession string) (string, error)
}

// Classify reports whether req is a browser remux this pool runs: Plex's
// transcoder, a DASH job Parse accepts, for a session Plex says Plex Web
// is playing. Any doubt leaves it to Plex.
func Classify(ctx context.Context, req *pb.ExecRequest, players Players) (Job, bool) {
	if req.GetTargetBinary() != "Plex Transcoder" {
		return Job{}, false
	}
	j, err := Parse(req.GetArgs(), req.GetEnv())
	if err != nil {
		return Job{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	product, err := players.PlayerProduct(ctx, j.SessionID())
	if err != nil || product != PlexWeb {
		return Job{}, false
	}
	return j, true
}
