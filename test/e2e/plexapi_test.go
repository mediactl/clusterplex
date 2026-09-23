//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	stateDir  = "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server"
	tokenFile = stateDir + "/.LocalAdminToken"
	plexLog   = stateDir + "/Logs/Plex Media Server.log"
	mediaDir  = "/media/Movies"
)

// plexPrelude is the Python that every call into a pod's Plex starts with.
// It talks to 127.0.0.1:32400, which in the pod namespace is the manager's
// proxy and behind it that pod's own Plex, with the local admin token Plex
// writes on a claimed server. The image carries no curl; Python it has.
const plexPrelude = `
import sys, re, time, urllib.request, urllib.parse, urllib.error
def _token():
    # The manager's choice: the server's own token, which every pod shares,
    # or the local admin token on a server that has not been claimed.
    try:
        m = re.search(r'PlexOnlineToken="([^"]+)"', open("` + prefsFile + `").read())
        if m: return m.group(1)
    except FileNotFoundError:
        pass
    return open("` + tokenFile + `").read().strip()
TOKEN = _token()
BASE = "http://127.0.0.1:32400"
def call(method, path, timeout=30, headers=None):
    h = {"X-Plex-Token": TOKEN, "Accept": "application/xml",
         "X-Plex-Client-Identifier": "clusterplex-e2e", "X-Plex-Product": "clusterplex-e2e",
         "X-Plex-Platform": "Chrome", "X-Plex-Version": "1.0", "X-Plex-Device": "Linux",
         "X-Plex-Device-Name": "clusterplex-e2e"}
    if headers: h.update(headers)
    req = urllib.request.Request(BASE + path, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
`

// execPodStdin is execPod with a program on standard input.
func execPodStdin(pod, stdin string, cmd ...string) (string, error) {
	c := exec.Command("kubectl", append([]string{"exec", "-i", "-n", namespace, pod, "--"}, cmd...)...)
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	return string(out), err
}

// plexPython runs a Python program against pod's own Plex and returns what it
// printed. The program has the prelude above in scope.
func plexPython(pod, program string) (string, error) {
	return execPodStdin(pod, plexPrelude+program, "python3", "-")
}

func mustPlexPython(t *testing.T, pod, program string) string {
	t.Helper()
	out, err := plexPython(pod, program)
	require.NoError(t, err, "python in %s failed: %s", pod, out)
	return out
}

// plexCall makes one request to pod's Plex and returns the status and body.
func plexCall(pod, method, path string) (int, string, error) {
	out, err := plexPython(pod, fmt.Sprintf(`
st, body = call(%q, %q)
print(st)
sys.stdout.write(body.decode("utf-8", "replace"))
`, method, path))
	if err != nil {
		return 0, out, err
	}
	first, rest, _ := strings.Cut(out, "\n")
	st, convErr := strconv.Atoi(strings.TrimSpace(first))
	if convErr != nil {
		return 0, out, fmt.Errorf("no status in %q", first)
	}
	return st, rest, nil
}

func mustPlexCall(t *testing.T, pod, method, path string) (int, string) {
	t.Helper()
	st, body, err := plexCall(pod, method, path)
	require.NoError(t, err, "%s %s on %s: %s", method, path, pod, body)
	return st, body
}

var (
	sectionRe = regexp.MustCompile(`<Directory[^>]*?\skey="(\d+)"[^>]*?\stype="movie"`)
	titleRe   = regexp.MustCompile(`<Video[^>]*?\stitle="([^"]*)"`)
	keyRe     = regexp.MustCompile(`<Video[^>]*?\sratingKey="(\d+)"[^>]*?\stitle="([^"]*)"`)
	partRe    = regexp.MustCompile(`<Part[^>]*?\skey="([^"]+)"[^>]*?\ssize="(\d+)"`)
	m3u8Re    = regexp.MustCompile(`session/[^\s]+/base/index\.m3u8`)
)

// movieSection returns the key of the movie library, creating one on
// mediaDir when the server has none, which is what a fresh cluster has.
func movieSection(t *testing.T, pod string) string {
	t.Helper()
	st, body := mustPlexCall(t, pod, "GET", "/library/sections")
	require.Equal(t, 200, st, body)
	if m := sectionRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	q := "name=Movies&type=movie&agent=tv.plex.agents.movie&scanner=Plex%20Movie&language=en-US&location=" +
		strings.ReplaceAll(mediaDir, "/", "%2F")
	st, body = mustPlexCall(t, pod, "POST", "/library/sections?"+q)
	require.Contains(t, []int{200, 201}, st, "creating the movie library: %s", body)
	st, body = mustPlexCall(t, pod, "GET", "/library/sections")
	require.Equal(t, 200, st, body)
	m := sectionRe.FindStringSubmatch(body)
	require.NotNil(t, m, "the movie library must exist after creating it: %s", body)
	return m[1]
}

// libraryTitles lists the titles pod's Plex holds in section.
func libraryTitles(pod, section string) ([]string, error) {
	st, body, err := plexCall(pod, "GET", "/library/sections/"+section+"/all")
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("listing section %s on %s: %d", section, pod, st)
	}
	var titles []string
	for _, m := range titleRe.FindAllStringSubmatch(body, -1) {
		titles = append(titles, m[1])
	}
	return titles, nil
}

// libraryItems maps the titles pod's Plex holds in section to rating keys.
func libraryItems(t *testing.T, pod, section string) map[string]string {
	t.Helper()
	st, body := mustPlexCall(t, pod, "GET", "/library/sections/"+section+"/all")
	require.Equal(t, 200, st, body)
	items := map[string]string{}
	for _, m := range keyRe.FindAllStringSubmatch(body, -1) {
		items[m[2]] = m[1]
	}
	return items
}

// mediaPart returns the key and size of an item's first media part.
func mediaPart(t *testing.T, pod, ratingKey string) (string, int64) {
	t.Helper()
	st, body := mustPlexCall(t, pod, "GET", "/library/metadata/"+ratingKey)
	require.Equal(t, 200, st, body)
	m := partRe.FindStringSubmatch(body)
	require.NotNil(t, m, "item %s must have a media part: %s", ratingKey, body)
	size, err := strconv.ParseInt(m[2], 10, 64)
	require.NoError(t, err)
	return m[1], size
}

// startTranscode starts an HLS transcode of ratingKey on pod, as a Plex web
// client would, and waits for its first segment. It returns the session and
// the path of the session playlist.
func startTranscode(t *testing.T, pod, ratingKey, session string) string {
	t.Helper()
	params := "path=%2Flibrary%2Fmetadata%2F" + ratingKey + "&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1" +
		"&directPlay=0&directStream=0&videoQuality=60&maxVideoBitrate=2000&videoResolution=1280x720" +
		"&hasMDE=1&location=lan&session=" + session
	st, body := mustPlexCall(t, pod, "GET", "/video/:/transcode/universal/decision?"+params)
	require.Equal(t, 200, st, "transcode decision: %s", body)
	st, body = mustPlexCall(t, pod, "GET", "/video/:/transcode/universal/start.m3u8?"+params)
	require.Equal(t, 200, st, "transcode start: %s", body)
	m := m3u8Re.FindString(body)
	require.NotEmpty(t, m, "the master playlist must name the session playlist: %s", body)
	index := "/video/:/transcode/universal/" + m + "?session=" + session
	require.Eventually(t, func() bool {
		st, size, err := fetchSegment(pod, index, session, 0)
		return err == nil && st == 200 && size > 0
	}, 60e9, 2e9, "the first segment of %s on %s", session, pod)
	return index
}

// fetchSegment reads the session playlist and one segment from it.
func fetchSegment(pod, index, session string, n int) (int, int, error) {
	out, err := plexPython(pod, fmt.Sprintf(`
index, session, n = %q, %q, %d
st, body = call("GET", index)
if st != 200:
    print(st, 0); sys.exit(0)
names = re.findall(r"^([^#\s].*\.(?:ts|m4s|mp4))$", body.decode("utf-8", "replace"), re.M)
if len(names) <= n:
    print(202, 0); sys.exit(0)
base = index.split("?")[0].rsplit("/", 1)[0]
st, seg = call("GET", base + "/" + names[n] + "?session=" + session)
print(st, len(seg))
`, index, session, n))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, out)
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("unexpected output %q", out)
	}
	st, _ := strconv.Atoi(f[0])
	size, _ := strconv.Atoi(f[1])
	return st, size, nil
}

// stopTranscode ends a session; a failure here is not a test failure.
func stopTranscode(pod, session string) {
	_, _, _ = plexCall(pod, "GET", "/video/:/transcode/universal/stop?session="+session)
}

// countInPlexLog counts lines of pod's Plex log matching pattern.
func countInPlexLog(pod, pattern string) (int, error) {
	out, err := tryExecPod(pod, "sh", "-c", fmt.Sprintf(`grep -a -c %s %q || true`, shellQuote(pattern), plexLog))
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("unexpected grep output %q", out)
	}
	return n, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// plexPID finds the Plex Media Server process in pod by name. The pid file
// is not used: Plex re-execs itself through vfork, so the pid it wrote is
// the process that exited, and the number can have been reused since.
func plexPID(t *testing.T, pod string) string {
	t.Helper()
	out := execPod(t, pod, "sh", "-c",
		`for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = "Plex Media Serv" ] && echo ${d#/proc/} && exit 0; done; exit 1`)
	pid := strings.TrimSpace(out)
	require.NotEmpty(t, pid, "%s: no Plex Media Server process", pod)
	return pid
}
