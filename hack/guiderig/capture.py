# capture.py <fixture> <tokenfile>: reads PMS's log on stdin and fetches
# each guide-service grid week and the lineup the moment PMS requests it,
# so the fixture is the service's answer of the same second (an airing that
# ends between two reads is in one and not the other; docs/guide-rig.md).
# Metadata ids are only recorded (meta-ids.txt), fetched after the load,
# paced, by fetchmeta.py. The token file holds a Plex account or server
# token and is read, never printed.
#
#   kubectl -n media logs -f plex-0 -c pmstail | python3 -I hack/guiderig/capture.py fixture tokenfile
#
# capture.log carries each request as PMS logged it (PMS-REQ <PMS's time>
# <file>) and each fetch's start, length and outcome: hack/guidebuild reads
# the request times as the moment each channel was read.
import json, os, re, sys, threading, time, urllib.parse, urllib.request

out, tokfile = sys.argv[1], sys.argv[2]
token = open(tokfile).read().strip()
os.makedirs(out + '/grid', exist_ok=True)
os.makedirs(out + '/metadata', exist_ok=True)
log = open(out + '/capture.log', 'a', buffering=1)
meta = open(out + '/meta-ids.txt', 'a', buffering=1)
REQ = re.compile(r'^(\w+ \d+, \d+ [\d:.]+) .*HTTP requesting GET (https://epg\.provider\.plex\.tv/\S+)')


def fetch(url, path):
    u = urllib.parse.urlsplit(url)
    q = [(k, token if k in ('X-Plex-Token', 'X-Plex-Api-Token') else v)
         for k, v in urllib.parse.parse_qsl(u.query, keep_blank_values=True)]
    real = urllib.parse.urlunsplit((u.scheme, u.netloc, u.path, urllib.parse.urlencode(q), ''))
    t0 = time.time()
    try:
        req = urllib.request.Request(real, headers={'Accept': 'application/json', 'X-Plex-Token': token})
        with urllib.request.urlopen(req, timeout=30) as r:
            body = r.read()
        with open(path, 'wb') as f:
            f.write(body)
        log.write(f'{t0:.3f} {time.time()-t0:.3f} ok {os.path.basename(path)} {len(body)}\n')
    except Exception as e:  # noqa: BLE001 - a scratch capture, every failure is logged
        log.write(f'{t0:.3f} fail {os.path.basename(path)} {e}\n')


for line in sys.stdin:
    m = REQ.match(line)
    if not m:
        if 'EPG[cloud]' in line or 'Step ' in line:
            log.write('PMS ' + line)
        continue
    when, url = m.group(1), m.group(2)
    u = urllib.parse.urlsplit(url)
    q = dict(urllib.parse.parse_qsl(u.query))
    if u.path == '/grid/bulk':
        path = f"{out}/grid/{q['channelGridKey']}-{q['startDate']}.json"
    elif re.match(r'^/lineups/[^/]+/channels$', u.path):
        path = f'{out}/lineup-channels.json'
    elif u.path.startswith('/library/metadata/'):
        meta.write(f'{when}|{u.path.rsplit("/", 1)[1]}\n')
        continue
    else:
        log.write(f'other {url}\n')
        continue
    log.write(f'PMS-REQ {when} {os.path.basename(path)}\n')
    threading.Thread(target=fetch, args=(url, path), daemon=True).start()
