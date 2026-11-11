# xmltv-sortprobe.py <out>: an XMLTV of one movie per character, titled "Z<c>Z" and "<c>Z", on the
# rig's four channels (9001-9004), so PMS's load writes each character's title_sort: the probe that
# settled guidebuild's sortTitle for an XMLTV guide (2026-11-11). Latin-1 Supplement and Latin
# Extended-A letters, a few of Extended-B and IPA, Greek, Cyrillic, ligatures, full-width letters and
# the punctuation a title may start with.
import datetime as dt, html, sys
out = sys.argv[1]
now = dt.datetime.now(dt.timezone.utc).replace(minute=0, second=0, microsecond=0) + dt.timedelta(hours=1)
chars = [chr(c) for c in range(0xC0, 0x180) if chr(c).isalpha()]
chars += list("ƀƁƆƏƐƑƒƓƔƖƗƘƙƚƝƟƠơƤƦƧƨƩƮƯưƱƲƳƴƵƶƷǍǎǏǐǑǒǓǔǕǖǗǘǙǚǛǜǝǞǟǢǣǤǥǦǧǨǩǪǫǰǴǵǸǹǺǻǼǽǾǿȘșȚțɐɑɒɔəɛɨɪʃʊʌʒ")
chars += list("ΑΒΓΔαβγδΩω") + list("АБВабвЖжЯя") + ["ﬁ", "ﬂ", "Ａ", "ｚ", "ẞ", "Ḃ", "ḃ", "Ẁ", "ẁ", "Ỳ", "ỳ"]
chars += list("\"'([{.¡¿“”‘’«»…#$&*!-_@~+=%^<>?/\\|;:,`·•–—×÷0123456789")
titles = []
for c in chars:
    titles += ["Z" + c + "Z", c + "Z"]
chans = ["9001", "9002", "9003", "9004"]
xml = ['<?xml version="1.0" encoding="UTF-8"?>', '<tv generator-info-name="clustarr-sortprobe">']
for n, name in zip(chans, ["X1 One", "X1 Two", "X1 Three", "X1 Four"]):
    xml.append(f'<channel id="{n}"><display-name>{name}</display-name></channel>')
for i, t in enumerate(titles):
    s = now + dt.timedelta(minutes=10 * (i // len(chans)))
    e = s + dt.timedelta(minutes=10)
    xml.append(f'<programme start="{s.strftime("%Y%m%d%H%M%S +0000")}" stop="{e.strftime("%Y%m%d%H%M%S +0000")}" channel="{chans[i % len(chans)]}"><title>{html.escape(t, quote=True)}</title></programme>')
xml.append('</tv>')
open(out, "w", encoding="utf-8").write("\n".join(xml) + "\n")
print(len(titles), "titles from", now.isoformat())
