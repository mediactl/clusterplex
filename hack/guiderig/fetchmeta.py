# fetchmeta.py <fixture> <tokenfile>: fetches every show's and movie's
# metadata PMS asked for during a load (the fixture's meta-ids.txt, written
# by capture.py), at most 3 a second, skipping the ones already fetched. A
# failed id is tried four times, then counted. The token is read from the
# file, never printed.
#
#   python3 -I hack/guiderig/fetchmeta.py fixture tokenfile
import os, sys, time, urllib.request

out, tokfile = sys.argv[1], sys.argv[2]
token = open(tokfile).read().strip()
ids = []
for line in open(out + '/meta-ids.txt'):
    i = line.strip().split('|')[-1]
    if i and i not in ids:
        ids.append(i)
fails = 0
for i in ids:
    path = f'{out}/metadata/{i}.json'
    if os.path.exists(path):
        continue
    url = f'https://epg.provider.plex.tv/library/metadata/{i}?X-Plex-Language=en'
    for attempt in range(4):
        try:
            req = urllib.request.Request(url, headers={'Accept': 'application/json', 'X-Plex-Token': token})
            with urllib.request.urlopen(req, timeout=30) as r:
                open(path, 'wb').write(r.read())
            break
        except Exception as e:  # noqa: BLE001 - scratch: retry, then count the failure
            if attempt == 3:
                fails += 1
                print('fail', i, e)
            time.sleep(5)
    time.sleep(0.34)
print(f'{len(ids)} ids, {fails} failed')
