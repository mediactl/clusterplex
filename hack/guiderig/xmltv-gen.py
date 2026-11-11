# xmltv-gen.py <out> <base URL>: the rig's 48-programme XMLTV probe (DVR 8, 2026-11-10), whose
# PMS load is the reference hack/guidebuild -xmltv compares with; a verbatim copy of gv/x1/gen.py.
import json, datetime as dt, html, sys, os
out = sys.argv[1]; base = sys.argv[2]  # e.g. http://10.96.x.y/hdhr/x1
now = dt.datetime.now(dt.timezone.utc).replace(minute=0, second=0, microsecond=0) + dt.timedelta(hours=1)
chans = ["9001", "9002", "9003", "9004"]
PO = "https://metadata-static.plex.tv/c/gracenote/cdcc962e86c6c828c2061b994f624cc2.jpg"   # poster 2:3
BG = "https://metadata-static.plex.tv/e/gracenote/e23d9ae1bec70f60d5f09b260141226b.jpg"   # background 16:9
ST = "https://metadata-static.plex.tv/e/gracenote/e5cffb7270d66c45ed2845f47c500661.jpg"   # snapshot
LG = "https://metadata-static.plex.tv/0/683a142553/0335412ec706f851f7d628a64dd1d827.png"  # clearLogo
CL = "https://provider-static.plex.tv/a/epg/channels/logos/gracenote/ac4ffe27107b835cf4e6a7a5b5c4b7c0.png"
def e(s): return html.escape(s, quote=True)
tests = []  # (id, inner xml)
def T(tid, inner): tests.append((tid, inner))
T("soup", f'''<title lang="en">Element Soup</title><title lang="es">Sopa de Elementos</title><sub-title lang="en">The Sub Title</sub-title><desc lang="en">A description of the soup.</desc><credits><director>Dir Ector</director><actor role="Lead">Act Orone<image type="person">{ST}</image><url>https://example.org/actor</url></actor><actor role="Support" guest="yes">Act Ortwo</actor><writer>Wri Ter</writer><producer>Pro Ducer</producer><presenter>Pre Senter</presenter><guest>Gue St</guest></credits><date>20190304</date><category lang="en">Drama</category><category lang="en">Series</category><category lang="en">Comedy</category><keyword lang="en">heist</keyword><language>English</language><orig-language>German</orig-language><length units="minutes">42</length><icon src="{PO}" width="680" height="1000"/><icon src="{BG}" width="1920" height="1080"/><icon src="{ST}"/><url>https://example.org/soup</url><country>US</country><episode-num system="xmltv_ns">2.4.</episode-num><episode-num system="onscreen">S03E05</episode-num><episode-num system="dd_progid">EP01234567.0005</episode-num><episode-num system="original-air-date">2019-03-04</episode-num><video><present>yes</present><colour>yes</colour><aspect>16:9</aspect><quality>HDTV</quality></video><audio><stereo>stereo</stereo></audio><previously-shown start="20190304"/><last-chance>last</last-chance><subtitles type="teletext"><language>English</language></subtitles><rating system="MPAA"><value>PG-13</value></rating><rating system="VCHIP"><value>TV-14</value></rating><star-rating><value>7/10</value></star-rating><review type="text">Good.</review><image type="poster" size="3" orient="P" system="tmdb">{PO}</image><image type="backdrop" size="3" orient="L">{BG}</image><image type="still">{ST}</image>''')
T("title-only", '<title>Title Only</title>')
T("sub-title-only", '<title>Sub Title Only</title><sub-title>Its Episode</sub-title><desc>No numbers at all.</desc>')
T("onscreen-only", '<title>Onscreen Only</title><sub-title>Seven</sub-title><episode-num system="onscreen">S02E07</episode-num>')
T("onscreen-odd", '<title>Onscreen Odd</title><episode-num system="onscreen">Ep. 9</episode-num>')
T("ddprogid-ep", '<title>DD Progid EP</title><sub-title>Ep Sub</sub-title><episode-num system="dd_progid">EP00000001.0003</episode-num>')
T("ddprogid-mv", '<title>DD Progid MV</title><episode-num system="dd_progid">MV00000002.0000</episode-num>')
T("ddprogid-sh", '<title>DD Progid SH</title><episode-num system="dd_progid">SH00000003.0000</episode-num>')
T("ddprogid-sp", '<title>DD Progid SP</title><sub-title>Team A at Team B</sub-title><category>Sports event</category><category>Sports</category><episode-num system="dd_progid">SP00000004.0000</episode-num>')
T("oad-only", '<title>OAD Only</title><episode-num system="original-air-date">2020-05-06</episode-num>')
T("ns-and-movie-cat", '<title>NS And Movie Category</title><category>Movie</category><episode-num system="xmltv_ns">0.1.</episode-num>')
T("movie-typical", '<title>Movie Typical</title><desc>A film.</desc><date>1993</date><category>Movie</category><category>Comedy</category><icon src="%s"/>' % PO)
T("movie-full-date", '<title>Movie Full Date</title><date>20210315</date><category>Movie</category>')
T("icon-land-first", '<title>Icon Landscape First</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="%s" width="1920" height="1080"/><icon src="%s" width="680" height="1000"/>' % (BG, PO))
T("icon-poster-first", '<title>Icon Poster First</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="%s" width="680" height="1000"/><icon src="%s" width="1920" height="1080"/>' % (PO, BG))
T("icon-nosize-two", '<title>Icon No Size Two</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="%s"/><icon src="%s"/>' % (BG, PO))
T("image-elements", '<title>Image Elements</title><episode-num system="xmltv_ns">0.0.</episode-num><image type="poster">%s</image><image type="backdrop">%s</image><image type="still">%s</image>' % (PO, BG, ST))
T("image-elements-movie", '<title>Image Elements Movie</title><category>Movie</category><image type="poster">%s</image><image type="backdrop">%s</image>' % (PO, BG))
T("icon-and-image", '<title>Icon And Image</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="%s"/><image type="backdrop">%s</image>' % (PO, BG))
T("icon-private", '<title>Icon Private</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="http://10.96.1.1/private.png"/>')
T("sports-live", '<title>Sports Live</title><sub-title>Team C vs Team D</sub-title><category>Sports event</category><category>Sports</category><category>Football</category><live/><new/>')
T("live-only", '<title>Live Only</title><episode-num system="xmltv_ns">0.0.</episode-num><live/>')
T("premiere-new", '<title>Premiere New</title><episode-num system="xmltv_ns">0.0.</episode-num><premiere/><new/>')
T("new-only", '<title>New Only</title><episode-num system="xmltv_ns">0.0.</episode-num><new/>')
T("prev-shown", '<title>Prev Shown</title><episode-num system="xmltv_ns">0.0.</episode-num><previously-shown start="20200101"/>')
T("prev-shown-bare", '<title>Prev Shown Bare</title><episode-num system="xmltv_ns">0.0.</episode-num><previously-shown/>')
T("nothing-flagged", '<title>Nothing Flagged</title><episode-num system="xmltv_ns">0.0.</episode-num>')
T("ratings", '<title>Ratings Only</title><episode-num system="xmltv_ns">0.0.</episode-num><rating system="VCHIP"><value>TV-MA</value></rating><rating system="MPAA"><value>R</value></rating><star-rating system="imdb"><value>8.5/10</value></star-rating><star-rating><value>3/4</value></star-rating>')
T("rating-noval", '<title>Rating No System</title><category>Movie</category><rating><value>PG</value></rating>')
T("credits-rich", '<title>Credits Rich</title><episode-num system="xmltv_ns">0.0.</episode-num><credits><director>D One</director><director>D Two</director><actor role="Hero">A One</actor><actor>A Two</actor><actor>A Three</actor><writer>W One</writer><producer>P One</producer><presenter>H One</presenter><commentator>C One</commentator><guest>G One</guest><composer>M One</composer><editor>E One</editor></credits>')
T("genres-many", '<title>Genres Many</title><episode-num system="xmltv_ns">0.0.</episode-num><category>Action</category><category>Adventure</category><category>Sci-Fi</category><category>News</category><category>Children</category><category>Talk</category><category>Reality</category><category>Documentary</category>')
T("year-and-oad", '<title>Year And OAD</title><date>2018</date><episode-num system="xmltv_ns">1.2.</episode-num><episode-num system="original-air-date">2018-11-20</episode-num>')
T("ids-tvdb", '<title>IDs TVDB</title><episode-num system="xmltv_ns">0.0.</episode-num><episode-num system="thetvdb.com">81189</episode-num><episode-num system="thetvdb.com/series">81189</episode-num>')
T("ids-tmdb", '<title>IDs TMDB</title><category>Movie</category><episode-num system="themoviedb.org">603</episode-num><episode-num system="imdb.com">tt0133093</episode-num><episode-num system="tms">MV000000000000</episode-num>')
T("ids-plex", '<title>IDs Plex</title><episode-num system="xmltv_ns">0.0.</episode-num><episode-num system="plex">plex://episode/5fc6914e1a65df002dd28cd6</episode-num><episode-num system="tv.plex.agents.series">plex://show/5d9c08a8e9d4a4001e7ae8d4</episode-num>')
T("guid-url", '<title>GUID URL</title><episode-num system="xmltv_ns">0.0.</episode-num><url system="plex">plex://movie/5fc6914e1a65df002dd28cd6</url><url>https://www.themoviedb.org/movie/603</url>')
T("keyword-country", '<title>Keyword Country</title><episode-num system="xmltv_ns">0.0.</episode-num><keyword>heist</keyword><keyword>bank</keyword><country>GB</country><language>French</language><length units="minutes">25</length>')
T("video-audio", '<title>Video Audio</title><episode-num system="xmltv_ns">0.0.</episode-num><video><quality>HDTV</quality><aspect>16:9</aspect></video><audio><stereo>dolby digital</stereo></audio><subtitles type="onscreen"/>')
T("repeat-a", '<title>Repeat Twice</title><sub-title>Same Ep</sub-title><episode-num system="xmltv_ns">0.3.</episode-num>')
T("repeat-b", '<title>Repeat Twice</title><sub-title>Same Ep</sub-title><episode-num system="xmltv_ns">0.3.</episode-num>')
T("series-ep2", '<title>Repeat Twice</title><sub-title>Other Ep</sub-title><episode-num system="xmltv_ns">0.4.</episode-num>')
T("same-title-noid-a", '<title>Same Title No Id</title><desc>First.</desc>')
T("same-title-noid-b", '<title>Same Title No Id</title><desc>Second.</desc>')
T("part", '<title>Part Episode</title><episode-num system="xmltv_ns">0.5.0/2</episode-num>')
T("season-only", '<title>Season Only</title><episode-num system="xmltv_ns">3..</episode-num>')
T("episode-only", '<title>Episode Only</title><episode-num system="xmltv_ns">.7.</episode-num>')
T("empty-ns", '<title>Empty NS</title><episode-num system="xmltv_ns">..</episode-num>')
T("unicode", '<title>Ünïcødé — “Quoted” &amp; More</title><sub-title>Ep: ½ &lt;tag&gt;</sub-title><episode-num system="xmltv_ns">0.0.</episode-num>')

slots = []
i = 0
for t in tests:
    ch = chans[i % len(chans)]; slot = i // len(chans)
    s = now + dt.timedelta(hours=slot); e_ = s + dt.timedelta(hours=1)
    slots.append({"id": t[0], "channel": ch, "start": s.isoformat(), "beginsAt": int(s.timestamp())})
    i += 1
xml = ['<?xml version="1.0" encoding="UTF-8"?>', '<tv generator-info-name="clustarr-x1" source-info-name="x1">']
xml.append(f'<channel id="9001"><display-name>X1 One</display-name><icon src="{CL}"/></channel>')
xml.append(f'<channel id="9002"><display-name>X1 Two</display-name><display-name>9002 X1 Two</display-name><display-name>X1TWO</display-name><icon src="{PO}" width="680" height="1000"/><icon src="{LG}" width="800" height="310"/><url>https://example.org/ch2</url></channel>')
xml.append('<channel id="9003"><display-name>X1 Three</display-name></channel>')
xml.append('<channel id="9004"><display-name>X1 Four</display-name><icon src="http://10.96.1.1/logo.png"/></channel>')
for t, sl in zip(tests, slots):
    s = dt.datetime.fromisoformat(sl["start"]); e_ = s + dt.timedelta(hours=1)
    xml.append(f'<programme start="{s.strftime("%Y%m%d%H%M%S +0000")}" stop="{e_.strftime("%Y%m%d%H%M%S +0000")}" channel="{sl["channel"]}">{t[1]}</programme>')
xml.append('</tv>')
os.makedirs(out, exist_ok=True)
open(f"{out}/xmltv.xml", "w", encoding="utf-8").write("\n".join(xml) + "\n")
json.dump(slots, open(f"{out}/../../slots.json", "w"), indent=1)
json.dump({"FriendlyName": "clustarr x1", "Manufacturer": "Silicondust", "ModelNumber": "HDTC-2US", "FirmwareName": "hdhomeruntc_atsc", "FirmwareVersion": "20150826", "DeviceID": "CAFE0011", "DeviceAuth": "clustarr", "TunerCount": 2, "BaseURL": base, "LineupURL": base + "/lineup.json"}, open(f"{out}/discover.json", "w"))
json.dump({"ScanInProgress": 0, "ScanPossible": 1, "Source": "Cable", "SourceList": ["Cable"]}, open(f"{out}/lineup_status.json", "w"))
json.dump([{"GuideNumber": c, "GuideName": "X1 " + c, "URL": base + "/stream/" + c} for c in chans], open(f"{out}/lineup.json", "w"))
open(f"{out}/device.xml", "w").write(f'<?xml version="1.0" encoding="UTF-8"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><URLBase>{base}</URLBase><device><deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType><friendlyName>clustarr x1</friendlyName><manufacturer>Silicondust</manufacturer><modelName>HDTC-2US</modelName><modelNumber>HDTC-2US</modelNumber><serialNumber>CAFE0011</serialNumber><UDN>uuid:CAFE0011</UDN></device></root>')
print(len(tests), "tests;", "first slot", now.isoformat(), "last", slots[-1]["start"])
