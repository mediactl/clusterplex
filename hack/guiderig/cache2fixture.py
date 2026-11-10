# cache2fixture.py <fixture>: turns the manager's cloud-guide cache, read
# as "key|base64 body" rows on stdin (psql -tA over clusterplex.epg_cache),
# into hack/guidebuild's fixture layout: lineup-channels.json,
# grid/<gridKey>-<start>.json and metadata/<ratingKey>.json. A body's
# base64 may cross lines; a row begins where a line's first field is a
# cache key.
import base64
import sys

F = sys.argv[1]
rows, cur, parts = [], None, []
for l in sys.stdin.read().split('\n'):
    if '|' in l and l.split('|', 1)[0].split('/', 1)[0] in ('grid', 'metadata', 'lineup'):
        if cur:
            rows.append((cur, ''.join(parts)))
        cur, first = l.split('|', 1)
        parts = [first]
    elif cur is not None:
        parts.append(l)
if cur:
    rows.append((cur, ''.join(parts)))
for k, b in rows:
    if k.startswith('lineup/'):
        p = F + '/lineup-channels.json'
    elif k.startswith('grid/'):
        _, gk, s, _e = k.split('/')
        p = F + '/grid/' + gk + '-' + s + '.json'
    else:
        p = F + '/metadata/' + k.split('/', 1)[1] + '.json'
    open(p, 'wb').write(base64.b64decode(b))
print(f'{len(rows)} cache rows written under {F}')
