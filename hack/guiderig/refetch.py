# refetch.py <fixture> <tokenfile> <lineup id> [gridKey:start:end ...]: the
# lineup's stations and any grid weeks a capture missed (a DNS failure, or
# a 401 on a request PMS signed with X-Plex-Api-Token, which the service
# refuses from anyone but PMS), sent with X-Plex-Token. Each week is
# written as grid/<gridKey>-<start>.json, as capture.py names it. The
# answer is the service's of now, not of the load's second, so an airing
# that ended between the two may differ from PMS's; hack/guidebuild reads
# the capture.log's request time for the channel either way.
#
#   python3 -I hack/guiderig/refetch.py fixture tokenfile 657c8183afd0f02cd353633f 657c-abc:2026-11-10:2026-11-17
import sys, urllib.request

F, tok, lineup = sys.argv[1], open(sys.argv[2]).read().strip(), sys.argv[3]


def get(url, path):
    req = urllib.request.Request(url, headers={'Accept': 'application/json', 'X-Plex-Token': tok})
    with urllib.request.urlopen(req, timeout=60) as r:
        b = r.read()
    open(path, 'wb').write(b)
    print(path, len(b))


B = 'https://epg.provider.plex.tv'
get(f'{B}/lineups/{lineup}/channels?X-Plex-Language=en', F + '/lineup-channels.json')
for gk, s, e in (a.split(':') for a in sys.argv[4:]):
    get(f'{B}/grid/bulk?channelGridKey={gk}&startDate={s}&endDate={e}&X-Plex-Language=en', f'{F}/grid/{gk}-{s}.json')
