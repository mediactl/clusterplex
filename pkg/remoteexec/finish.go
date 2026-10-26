package remoteexec

import pb "github.com/mediactl/clusterplex/proto"

// Finish tells the shim how a job ended, as Plex's transcoder's exit code:
// with err, "<prefix>: <err>" on stderr and exit 1; without, exit 0. It
// returns err.
func Finish(sink Sink, prefix string, err error) error {
	code := int32(0)
	if err != nil {
		code = 1
		_ = sink.Send(&pb.TranscodeLog{StderrChunk: []byte(prefix + ": " + err.Error() + "\n")})
	}
	_ = sink.Send(&pb.TranscodeLog{ExitCode: code, IsFinished: true})
	return err
}
