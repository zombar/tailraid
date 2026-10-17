# 18 — Search, the index in the volume and the media catalogue

One search box finds files and folders by name across the volume, and text files and
audio tracks by what is in them; a Media tab (13 §10) browses films, shows and music by
poster and cover with the descriptions and art the volume holds. Names need no index:
every node has the whole tree in memory. Contents and media metadata are read once, by
the leader, into an **index kept inside the volume** under `/.tailraid/index`, so every
node reads the same index as it reads any other file, and a device that joins tomorrow
has it at once. This reverses the "search" non-goal 13 §9 carried until now.

*Status: `tailraid media scrape` (§11) is built (2026-10-13). (2026-10-11): designed; docs, invariants, lookdev and stubs first. I-206..I-212
are stubs in `test/invariants/` until each PR lands its tests: `library-flag` (I-210),
`search-index` (I-206, I-207, I-211), `search-web` (I-208, I-209), `media-web` (I-212),
`search-soak` (phase 21).*

## 1. Scope

- **Names**, live from the FSM: every live file and folder, matched word by word.
- **Text contents**: the first `search.max_file` bytes of a text file (by extension),
  indexed as words.
- **Audio tags**: title, artist, album and genre from a track's first chunk.
- **The media catalogue**: one entry per film, show, season, episode or album under a
  folder flagged as a library (§9), from Kodi/Jellyfin sidecars (`.nfo`, `poster.*`,
  `fanart.*`), embedded tags and the names themselves. No node fetches metadata from the
  internet: a node never talks to a provider, and an item without sidecars shows its
  cleaned name and the browser-made thumbnail (13 §5). Sidecars come from the user's
  own tools or from `tailraid media scrape` (§11), a CLI run that asks TMDB and
  MusicBrainz from the user's device and writes the missing sidecars into the volume.
- Everything is written by the leader only, as immutable files, and read by any node.

## 2. What is indexed (I-207)

Files only, keyed by inode (`Ino` is never reused, 00 §Glossary, I-3), so a rename, a
move, a delete to the trash or a restore needs no re-index (a file in the trash is indexed
like any other and only the query's live filter hides it, so the pass follows the change
feed file by file, §5), and a file is re-indexed only when its content changed: the doc
table records `sha256(Chunks)`, the hash of the chunk list, and a candidate is one whose
hash is absent or different. `Version` alone would not do: it bumps on `chmod` and
`utimes` too, which rsync and Finder issue constantly.

- *Text*: the extensions the file view treats as text (13 §1: `md markdown txt csv tsv
  json yaml yml toml ini conf log srt vtt html htm xml go ts js py sh rs c h java rb`),
  the first `search.max_file` bytes (default 1 MiB; at least 64 KiB, at most one chunk),
  decoded as UTF-8 with invalid sequences dropped. A larger file is indexed by name only.
- *Audio* (`mp3 m4a flac ogg opus aac wav`): the first chunk only, parsed by the tag
  reader of 13 §6 (`internal/mediatag`, moved out of the web handler); a tag that does not
  fit in the first chunk or does not parse leaves the track indexed by name only, never
  an error. The display strings `title`, `artist`, `album` are stored with the doc so a
  hit can show them.
- *Excluded*: everything under `/.tailraid`, deleted roots and their subtrees (the
  live-only `GetInode`, 02 §Apply rules), and `IsEphemeral` names (upload temps, macOS
  metadata, 06 §1).
- *Bounds*: a pass takes at most `search.batch` (2000) candidates or `search.batch_bytes`
  (64 MiB) of text, whichever comes first, so the postings a pass holds in memory while it
  writes its segment are bounded by a number the operator sets, not by the files found;
  above `search.max_bytes` (256 MiB of segments) the pass indexes tags, names and the
  catalogue only and `status` reports `capped`. A million small files is many passes,
  never a large one.

## 3. Tokens

Unicode letters and digits form words; a word is lowercased, NFKC-normalised and
stripped of combining marks (`golang.org/x/text/unicode/norm`, pure Go, vendored), so
`Café` and `cafe` are the same word; words shorter than two characters are dropped; there
is no stemming. The same function folds names and queries, so a search for `agnes`
finds `Agnès Obel` by name, tag and content alike. A CJK run is one word per character.

## 4. Segments (I-206)

There is no manifest. A pass writes one immutable **segment** `/.tailraid/index/<seq>.seg`
and nothing else; the newest complete segment names, in its header, the sequence numbers
of the segments it extends (its **chain**), so readers find the current index by listing
the folder. Every node keeps the segments in its chunk cache (the system pin, 01 §Local
chunk cache, I-373) and reads them through a reader that consults the cache without
filling it, so a search answers from the node's disk when the network is busy. The rules:

- The file is created with `O_CREATE|O_EXCL`: the create is the compare-and-swap. A
  leader demoted mid-pass still has its proposals forwarded (02 §Leader forwarding), so a
  second leader could otherwise write the same `<seq>`; it loses the create with `ErrExist`
  and abandons the pass. `<seq>` is the highest sequence in the folder plus one.
- The bytes are written to a temp sibling, **flushed** (`FS.Flush`, 17 §2: the Commit
  lands before the name is visible) and renamed, so another node never sees a short file;
  a trailer (`TRSI`, the body's length and its SHA-256) lets a reader refuse one anyway.
- Header: magic `TRSI`, version 1, the chain (`uvarint n`, `n × uvarint seq`), the FSM
  `Applied` index of the pass (display). Body: the **doc table** (`uvarint n`, then per doc
  `ino u64, gen u32, chunks-hash 32 B, kind u8, title, artist, album` as strings), the
  **term dictionary** (sorted, front-coded: `uvarint shared prefix, string suffix`,
  `uvarint postings offset`), the **postings** (per term `uvarint n`, then `uvarint doc
  delta, uvarint tf, uvarint first byte offset`), and the **catalogue** (§8: `uvarint n`,
  then per entry its fields). Everything is varints: a segment of one pass stays under
  one chunk for a batch of ordinary files, which is one read on a cold node.
- Readers (`internal/search.Index`) `ReadDir` the folder (sorted, 01), take the highest
  segment whose trailer verifies, load it and its chain, and keep each segment as one
  `[]byte` searched in place: the dictionary by binary search, a posting list decoded
  when a query touches it, so a node's memory for the index is the segments' bytes once,
  not three times. A reader checks the folder's `(ino, version)` before a query and
  reloads when it moved. A segment that fails its trailer is skipped and its docs count
  as unindexed, which the next pass repairs.
- A pass with nothing changed writes nothing. When the chain would exceed `RebuildAt` (8)
  segments the pass merges the chain and its batch into one segment with no chain: the
  dictionaries are walked in step, each inode keeps its newest doc while the file is
  live, and only the output is held in memory, so a reader's chain is at most 8 files
  and a deleted file's words leave the index at the next rebuild.
- `/.tailraid` is ephemeral (06 §3, I-195): a delete or replace never trashes. The
  leader's hourly sweep removes a `.seg` outside the current chain once it is an hour
  old, which is how a rebuild's predecessors and an abandoned pass's temp go.

## 5. The leader's pass

On the maintenance tick (`node.go` `maintenance`, next to GC) every `search.interval`
(10 min, `"0"` off) when `Applied` advanced since the last pass: load the chain and
collect the candidates from the FSM's change feed (02 §The change feed). The pass keeps a
pending set: every inode the feed reported since the last pass, with the files under a
changed folder (a restored or renamed tree). It checks each against the chain, and one the
chain already holds at its chunk list and generation leaves the set. The pass walks every
inode (`Store.ForEachInode`: keys sorted, copies taken in batches of 1000 under the read
lock) only when the feed cannot answer (a new leader, a node started from a snapshot, a
feed overflow) and once every `SweepEvery` (an hour), which also catches what no change
reports (a lifted size cap). From the candidates in inode order it takes at most `search.batch` of them or
`search.batch_bytes` of text, read them `search.workers` (2) at a time under
`chunk.Background` (06 §4, I-175), tokenize, derive
the catalogue entries (§8), write the segment (§4). A file it did not take or could not
read stays pending. The catalogue is rebuilt only for the libraries a change lies under
(every library on a changed inode's ancestor chain, so nested libraries rebuild together);
the others keep their entries from the chain's newest catalogue, and a change to the set of
libraries rebuilds every one. A pass with no candidate and no library touched reads
nothing and writes nothing (I-387). While candidates are pending the
next pass runs after `Backlog` (30 s) instead of the interval, so a large import is
indexed pass after pass rather than one batch per interval. The leader remembers each
`.nfo` it parsed by its chunk list, so a pass re-reads only changed sidecars; a new
leader reads them once. `AdminOp "index_now"` (`IsPass`, I-189) runs a pass on demand: `tailraid search
index` and the soak use it. `status` carries `search: {docs, segments, bytes, pending,
last_pass, in_progress, capped}` (12 §8). The dashboard's Volume card shows it as the Index tile (13 §1): the files indexed, the index's size and segments, what is pending and whether it is capped.

## 6. Queries (I-208)

`GET /api/search?q=&under=&limit=` (13 §3). The query is folded into words (§3); words are
ANDed; the last word matches as a prefix (typeahead), every other word exactly; `under`
limits hits to a live folder's subtree (its own path included); `limit` is 50 by default,
200 at most, and 0 or more than 200 is 400 (`{0, 1, max−1, max, max+1}`).

A hit is `{path, type, size, mtime, ino, match, title?, artist?, album?, terms:
[{term, off}]}` with `match` one of `name`, `tags`, `content` (and `field`, §6a):

- **Names** are matched live against the FSM, so a file written a second ago is found by
  its name at once. The node keeps a folded name list (every inode's words) that a query
  first brings up to date from the change feed: an inode changed since the last update is
  read again, and a deleted one is dropped. It is built by a walk only when the feed cannot
  answer, never on every keystroke, even while a copy keeps the FSM moving (I-388). A query
  then scans the list, one pass over a byte slice. The trash, `/.tailraid` and ephemeral
  names are filtered per hit, as below.
- **Tags** and **content** come from the segments. Every candidate inode is filtered
  through the live `GetInode` (nothing in the trash or under `/.tailraid` is a hit) and
  `PathOf` names it.
- **Order**: name hits, then tag hits, then content hits; within a group by the sum of
  term frequencies descending, then path ascending, then inode; `limit` applies after the
  whole sort. **Determinism**: two nodes with the same chain and the same FSM return
  byte-identical bodies (the N-node test).
- A node whose chain fails to load answers the name hits with `"index": "unavailable"`.

## 6a. Fields: one grammar, two scopes (I-411..I-413)

A query is words and **field terms**, parsed once by `search.ParseQuery` for both search
fields of the page (13 §3 and §10d), the CLI's `tailraid search` and every scope:

| Term | Matches a catalogue item whose |
|---|---|
| `actor:<v>` | actors (the people after its directors, §8) |
| `director:<v>` | directors |
| `person:<v>` | actors or directors |
| `genre:<v>` | genres |
| `title:<v>` | title or sort title |
| `artist:<v>` | artist (an album's, or a track's album's) |
| `year:<n>`, `year:<nnnx>`, `year:<a>-<b>` | year is `n`, in the decade, or in `[a, b]` |
| `kind:<k>` | kind: `movie`, `show`, `episode`, `album`, `game` |
| a bare word | title, people, genres, artist or plot (the last bare word as a prefix) |

`<v>` is one word or a `"quoted phrase"`; it is folded as the index folds (§3). An
unquoted value matches a field value that has the word among its words; a quoted one
matches a value whose folded form equals it (`actor:"tom hanks"` is Tom Hanks and never
Tom Hanks Jr.). A bare token that folds to no word (a single letter while the viewer types)
is skipped. Terms are ANDed over **items**: each term's matching entries are lifted to their
item first (a season or an episode to its show), so an actor named in an episode's sidecar
and a genre in its show's meet in the show; a field named twice is two conditions. A query is at most
`QueryMax` (1 KiB) and `TermsMax` (16) terms; an unknown field, an unclosed quote, an empty
value, a malformed year or more terms than that is a 400 that names the offset of the
fault (`{"error","at"}`), never a partial answer.

The node keeps a **field table** next to the name list: for each field, its folded values
with the catalogue items that carry them, built from the catalogue when the chain loads
or changes (it is never stored, as the name list is not), so a field term costs a map
lookup and a quoted value no scan. The table is a pure function of the catalogue: two
nodes with the same chain hold the same table.

The **scope** chooses what a hit is, never which items match:
- `scope=media` answers the matching catalogue items (films, shows, episodes as their
  show, albums, games), grouped by library in `Store.Libraries()` order and within each by
  `sort_title` then path, at most `limit` in all.
- `scope=files` (the header and the CLI) answers files: name, tag and content hits as
  before, and a query with field terms matches the files of the items its field terms
  match (a film's or an episode's video, an album's tracks, a game's files) that the
  query's words also match by name, tags or content; a query of field terms only lists
  those files as `match: "field"` hits, ordered by path.
A test runs every query of a table through both scopes and checks the media hits'
items are exactly the items of the files hits (I-412).

**Suggestions** (`GET /api/search/suggest`, I-413). For the last word of `q` as a prefix,
the values of each field whose folded words start with it and that, with the other terms
of `q`, match at least one item; per value the count of matching items by kind; fields
in the order actor, director, genre, title, artist; values by total count descending,
then folded value; at most `limit` (8 by default, 50 at most) per field. The answer is a
pure function of the table and `q`.

## 7. Snippets

No route. For a content hit the page fetches `GET /files/<path>` with `Range:
bytes=max(0, off−100)-off+140` (13 §3, I-69), `off` the stored first offset of the matched
term, three at a time as hits scroll into view, and marks the term. A chunk with
recorded sums serves that range from one data shard (04 §5).

## 8. The media catalogue (I-211)

The same pass derives one catalogue entry per media item under every folder whose
effective library kind (§9) is set, stored in the segment beside the docs:

- *Movies* (`movies`): each child folder of the library is a film, and so is each video
  file directly in it. Title and year come from the folder or file name — `Title (2019)`,
  or a scene name cut at its first release token (`1080p 720p 2160p 4k x264 x265 h264
  h265 hevc bluray webrip web-dl hdtv dvdrip remux aac dts atmos proper repack`), season
  or episode number (`S01`, `Season 2`, `Seasons 1 and 2`, `S01E02`) or year (after the
  first word and not followed by another year, which then is the year), with `[…]`
  dropped and dots and underscores as spaces, so a season pack's folder
  `Invasion.2021.S01.PROPER.WEBRip` is the show `Invasion` (2021) — and are overridden by
  `movie.nfo` or `<name>.nfo` (Kodi XML: `title`, `originaltitle`, `year`, `plot`,
  `outline`, `tagline`, `runtime`, `genre`, `rating` or `ratings/rating/value`, `mpaa`,
  `director`, `actor/name`). Art: `poster.*`, `<name>-poster.*`, `folder.*` for the
  poster, `fanart.*`, `<name>-fanart.*` for the background, `<name>-thumb.*` for the
  frame, else the browser-made thumbnail (13 §5).
- *TV* (`tv`): each child folder is a show (`tvshow.nfo`: `title`, `year`, `plot`,
  `genre`, `rating`, `status`; art `poster.*`, `fanart.*`, `banner.*`); `Season NN`,
  `Series NN` and `Specials` folders are seasons (`seasonNN-poster.*`, `season-specials-
  poster.*`); video files are episodes with `SnnEnn`, `nnxnn` or `Episode nn` in the name
  (`<name>.nfo`: `title`, `plot`, `aired`, `season`, `episode`, `runtime`; `<name>-thumb.*`
  else the browser-made thumbnail; a title that is only the show's name again becomes
  "Episode N"); a double episode `S01E01E02` is one entry spanning
  both numbers; an episode directly under the show belongs to the season its name says. The catalogue keeps an entry
  per folder; the Media tab shows sibling folders whose names clean to the same title
  and year as one show (13 §10).
- *Music* (`music`): artist folders of album folders as the album view defines them
  (13 §6: a folder whose audio files outnumber its other files). The catalogue stores
  album, artist, year, genre and track count from the first track's tags and the cover
  (`dir.cover`, I-165, else `cover.*`/`folder.*`), plus an `artist.nfo` biography and
  `artist.*`/`fanart.*` art when present.
- An `.nfo` is parsed with `encoding/xml` from its first `search.max_file` bytes; one that
  is not XML (Kodi allows a bare provider URL) or does not parse contributes nothing and
  is not an error. A field a sidecar lacks is taken from the name; a field neither has is
  absent.
- An entry is keyed by the item's inode and carries `kind, title, sort_title, year, plot,
  genres, rating, runtime, mpaa, people, season, episode, aired, poster, fanart, thumb`
  (`people` the directors first and then up to six actors, and from segment version 4
  `directors`, how many of them are directors, so `actor:` and `director:` can tell them
  apart; an entry of an older segment counts none and its people match `person:` only),
  art as volume paths relative to the item. The same rules on every node give the same
  catalogue (the determinism test), and a renamed or moved item changes nothing until a
  name-derived field differs.

### 8a. The name analyser (I-276)

Every name-derived field comes from one function, `search.Analyze(name, dirs)`: a file's
or folder's name and the names of the folders above it inside the library, nearest first.
It reads nothing else, so it is the same on every node. Its rules were written against a
real volume's 9.8k names (films, shows, music, ROMs; the shapes are in the table test
`internal/search/testdata/names.tsv`), not against an imagined scene:

| Field | Read from |
|---|---|
| title, year | `Title (2019)`, `Title [2019]`, a scene name cut at its first release token, season or episode number, language token or year (as above); a leading site (`www.UIndex.org - `), a trailing ` copy`, `[…]` groups (`[YTS.MX]`, `[y2flix.cc]`, `[6.5GB]`) and the release group after the last `-` are dropped |
| date | an air date `2024.03.14`, `2024-03-14` or `14.03.2024` |
| season, episode, last episode | `S01E02`, `S01E01E02`, `S01E01-E03`, `1x02`, `Episode 2`; a folder's `Season 2`, `Series 2`, `Specials`, `S02`, `Season.1`; a pack folder's range (`S01e01-10`, `Seasons 1 and 2`) names seasons, not an episode |
| episode title | the words after the code up to the first release or language token: `Severance - S01E01 - Good News About Hell`, `…2x01.Il.Circolo.Spezzato.ITA.ENG…` |
| part | `Part 01 of 03, The Overlords`: in a TV library a part is an episode of season 1 and the rest its title |
| absolute | an anime's ` - 012 ` or `E012` after a `[Group]` with no season |
| quality | resolution, source, codec and audio tokens (`1080p`, `WEBRip`, `x265`, `DDP5.1`, `HDR`, `10bit`), in name order |
| languages | `ITA ENG Spa`, `iTA-ENG`, No-Intro `(En,Fr,De)` |
| edition | Director's Cut, Extended, Unrated, Remastered, Deluxe Edition, Japanese Edition, Live |
| disc | `CD1`, `CD 2`, `D1`, `Disc 1`, `Disc I`, `DISC TWO`, `(2CD)` as a count, `(Disc 2 of 3)` |
| track | a leading `01 `, `01. `, `01 - `, `01 -`, `01_-_`, `1-01 ` of an audio file |
| regions, revision, flags | No-Intro and GoodTools tags: `(USA)`, `(EU - AU)`, `(Europe)`, `(Japan)`, `(Rev 2)`, `(v1.1)`, `[!]`, `(Proto)`, `(Beta)`, `(Hack)`, `[T+Eng]`, Switch `[DLC]`, `[UPD]` and a 16-hex title ID |

A folder also has a **role** that the catalogue uses before its name: *disc* (the names
above) merges into the album or game above it, numbered; *category* (`Albums`, `Studio
Albums`, `Singles`, `Compilations`, `1 Studio`, `2 - Compilations`, `Hacks`,
`Translations`, `Unreleased`, `Games`) and *format* (`MP3 CBR Original`, `[MP3 VBR]`)
are walked through, a category's word becoming a ROM flag; *region* (`USA`, `Japan`,
`Europe`, `EU`) is walked through and sets a ROM's region; *extras* (`Subs`, `Subtitles`,
`Sample`, `Extras`, `Featurettes`) is not looked into. So `YES/1 Studio/1983 90125
(Remastered and Expanded) @320` is the album *90125* (1983) by YES,
`Genesis/1974 The Lamb Lies Down On Broadway/D1` is one album with two discs, and
`Invasion.2021.S01.PROPER.WEBRip/Subs/Invasion.S01E01…` is no episode.

A catalogue entry gains `date, absolute, part, edition, quality, languages, track` and
`episode_title`; an episode with only a date or an absolute number has season -1 and is
ordered by it. The catalogue section of a segment carries its own version (2); a reader
takes version 1 with the old fields, and the leader's next pass rewrites the catalogue
because its bytes differ.

### 8b. Games (I-278)

Under a `roms` library (§9) each child folder is a **platform**, named from a table of
EmulationStation short names and common spellings (`n64`/`N64` Nintendo 64,
`snes`/`sfc`/`SNES` Super Nintendo, `gc`/`gamecube` GameCube, `psx`/`ps1` PlayStation,
`psp`, `switch`, `gb`, `gbc`, `gba`, `nds`, `3ds`, `wii`, `megadrive`/`genesis`,
`mastersystem`, `gamegear`, `segacd`, `32x`, `saturn`, `dreamcast`, `ps2`, `pcengine`,
`atari2600`, `atari7800`, `lynx`, `neogeo`, `arcade`/`mame`/`fbneo`, `msx`, `c64`,
`amiga`, `dos`; an unknown folder keeps its own name). Below it, region, category and
format folders are walked through. A **game** is a file whose extension the platform
takes (N64 `z64 n64 v64 zip 7z`, SNES `sfc smc zip 7z`, GameCube `iso rvz gcz ciso`,
PlayStation `pbp chd cue bin m3u iso`, PSP `iso cso pbp`, Switch `nsp xci nsz xcz`; the
table lists each platform), or a folder holding a `cue`, `m3u` or `chd` set; anything else
(`.apk`, a driver `.zip` under Switch, `Keys-*` folders, `*.keys`, `.txt`) is no game.
Files of one platform whose analysed names differ only in their disc are one game with
its discs. A Switch update or DLC (`[UPD]`, `[DLC]`, or a title ID not ending in `000`) is part of a game, not one, and is left out. Region and revision variants of one title are separate entries that the Media
tab shows as one card. Fields come from the name (§8a: title, regions, languages,
revision, flags, disc) and, when the platform folder holds one, an EmulationStation
`gamelist.xml` (`game/path`, `name`, `desc`, `releasedate`, `developer`, `publisher`,
`genre`, `players`, `rating`, `image`, `thumbnail`, `marquee`), read like an `.nfo` (the
first `search.max_file` bytes, `encoding/xml`, nothing when it does not parse). Box art is
`gamelist.xml`'s image, else `<stem>.png|jpg` beside the game, `images/<stem>*`,
`media/boxart/<stem>.*`, then `boxart.*` or `cover.*` in a game folder; else the Media tab
draws a title card (13 §5). A platform's art is its folder's own `platform.*`, `poster.*`,
`folder.*` or `logo.*`, else the box art of its first game by sort title that has one
(I-400), so a platform scraped with `media scrape --only games` shows a box, not a card. The ROM file is never read: no header parse, no hash, no
DAT match. The catalogue section of segment version 3 carries a game's platform, regions, revision, flags, developer, publisher and players; a platform's entry carries its game count.

## 9. Libraries (I-210)

A library is a folder flag like `protected` and `nsfw` (02 §Apply rules): `Inode.Flags`
gains a two-bit **library kind** (`FlagLibraryMask = 0b1100`: 0 none, 1 movies, 2 tv,
3 music), directories only, set by `SetAttr` with `AttrFlags`. Since codec 10 the field is
three bits (`FlagLibraryMask = 0b11100`) and kind 4 is `roms` (I-277): the values already
stored keep their bits, and a kind of 4 or more is refused with `ErrCodec` while a live
member reads codec 9 or older. It is **codec 6**: an
older binary rejects the unknown bits, so the leader refuses the command with `ErrCodec`
while any live member reads an older codec (the rule of I-156), which is what keeps every
FSM identical. The effective kind of a folder is the nearest flagged ancestor's (its own
first); a library inside a library is its own section. `ls` and `stat` carry `library`
next to `protected` and `nsfw`, `POST /api/flags` accepts `library` with one of the four
words, and the folder menu offers it (13 §4). `Store.Libraries()` lists every flagged
folder in kind-then-path order for the Media tab; no new CLI command is needed. It asks
for each request and each catalogue pass, so it reads a set of flagged directories that
the applier keeps (the one `SetAttr` that sets flags adds or drops a directory; a delete
drops it; `Restore` rebuilds it, never encoded) instead of walking every inode: O(libraries
× depth), whatever the volume holds (I-384).

## 10. Non-goals

Stemming, fuzzy matching, regular expressions, PDF or Office text, EXIF, content past
`search.max_file`, a shared query history, a node talking to a metadata provider (only
the CLI's scrape does, §11), providers beyond TMDB, MusicBrainz and the Cover Art Archive
(TVDB, fanart.tv, IGDB, ScreenScraper), a game's facts and description (a `gamelist.xml`
is never written), replacing a sidecar that exists,
a scheduled or automatic scrape, subtitles, transcoding, a music artist
page beyond the biography and art, launching or emulating a game, reading a ROM's header
and matching ROMs against DAT files.

## 11. Scraping sidecars: `tailraid media scrape` (I-298..I-302)

The catalogue (§8) is only as rich as the sidecars beside the media. `tailraid media
scrape` fills the gaps: run on any member device, it lists the library items that lack
a sidecar or art, looks each one up with **TMDB** (films, shows, seasons, episodes) or
**MusicBrainz and the Cover Art Archive** (album covers) or the **libretro thumbnails**
(a game's box art, by its No-Intro or Redump name), and writes the missing files
into the volume under the names §8 already reads. Nothing on the read side changes: the
next index pass (§5) picks the files up like ones a person or tinyMediaManager wrote.

**Where it runs.** Only the CLI process talks to a provider, from the device the user
runs it on; no node ever does (I-298), so the volume works offline and a node's
network surface stays the tailnet. The CLI asks its local node for the plan (the admin
op `scrape_plan`, paged), and writes each file through that node's web listener
(`PUT /files/<path>` with `If-None-Match: *`, the browser's exclusive upload of 13 §4,
I-226), so the device needs no mount; a node without `listen.web` is refused with that
reason.

**Commands.**

| Command | Does |
|---|---|
| `tailraid media scrape [path…]` | every library folder when no path is given, else the items under the paths; `--only movies,tv,episodes,music,games` (all by default), `--lang` (TMDB language, default `en-US`), `--region` (certification country, default `US`), `--yes`, `--dry-run`, `--id tmdb:<n>`, `--id mbid:<uuid>` or `--id libretro:<name>` for a single item path, `--again` (ask again about the items answered "no match"); its TMDB key (a v3 key, or a v4 read token) is the secret `tmdb` (`tailraid secret set tmdb`, docs/19 §6), checked with one `GET /3/configuration` before a run that needs it; with no key it does the keyless groups (`music`, `games`) and says it skipped the rest |

**The plan.** `scrape_plan` answers, for each item under the paths in catalogue order,
its path, kind, the analysed title and year (§8a), its seasons and episode files with
their numbers, an album's artist and title from its first track's tags and whether a
track carries a picture, and which of its **slots** already hold a file. A slot is one
thing the catalogue reads, and it is *present* when any name §8 accepts for it exists:

| Item | Slot | Written as (when missing) | Also counts as present |
|---|---|---|---|
| film folder | nfo · poster · fanart | `movie.nfo` · `poster.jpg` · `fanart.jpg` | `<video>.nfo`, `<folder>.nfo` · `<video>-poster.*`, `folder.*` · `<video>-fanart.*` |
| film file in the library | nfo · poster · fanart | `<stem>.nfo` · `<stem>-poster.jpg` · `<stem>-fanart.jpg` | |
| show | nfo · poster · fanart | `tvshow.nfo` · `poster.jpg` · `fanart.jpg` | `folder.*` |
| season | poster | `seasonNN-poster.jpg` (`season-specials-poster.jpg`) in the show folder | `poster.*`, `folder.*` in the season folder |
| episode | nfo · thumb | `<stem>.nfo` · `<stem>-thumb.jpg` | |
| album | cover | `cover.jpg` | a track's embedded picture, `cover.*`, `folder.*` |
| game file | boxart | `<stem>.png` beside it | the `gamelist.xml` image, `images/<stem>*`, `media/boxart/<stem>*`, `boxart/<stem>*` (§8b) |
| game disc set's folder | boxart | `boxart.png` inside it | `cover.*`, `folder.*` |

Images are the provider's own files at a fixed size (TMDB `w780` posters, `w1280`
backdrops, `w300` stills; the Cover Art Archive's `front-500`; libretro's `Named_Boxarts`
PNG), re-encoded never. A game is planned only on a platform whose libretro system the
platform table names (§8b; not the Switch); its facts and description stay with a
`gamelist.xml`, which a scrape never writes.

**Matching** (I-301, I-395). The query is the analysed title, with the year when the name has
one. Titles compare after the tokenizer of §3 (NFKC, case-folded, punctuation as
spaces), keeping every word however short (the index drops one-letter words; a match
cannot: `Tekken 6` is not `Tekken 5`), with a Roman numeral from II to XX read as its
number (`Final Fantasy VII` is `Final Fantasy 7`). A result is a **clear match** when it is the only result whose title or
original title equals the query's and, when the query has a year, whose year (a film's
release year, a show's first air year) equals it too; an album's clear match is the only
release group whose artist and title both equal the tags', and of several (an artist's
self-titled albums) the only one whose first release year is the tags' year. A game's box art is the set's name equal to
the file's own (a disc set's folder name) with each of ``&*/:`<>?\|`` as `_`, as libretro files
them; else the only name whose title (before its first tag) equals the game's analysed
title or the file's own; else the only one of those tagged with one of the game's regions, a short region
tag read as the set's word for it (`EU` Europe, `US` USA, `JP` Japan, `AU` Australia and
the rest of the table in `scrape.regionNames`), since a PSP or other disc dump is named
`Tekken 6 (EU)` while the set files `Tekken 6 (Europe)`.
The set's names come from its index page, fetched once per system a run. A clear match is used
without asking. Otherwise, on a terminal the CLI lists the top five (title, year, the
first line of the overview) and takes a number, `s` to skip this run, `n` for no match
or an id. A **no match** is remembered (I-394): the CLI sends the admin op
`scrape_skip` (the item's path and inode, needing Write there as a sidecar does) to its
node, which creates `/.tailraid/scrape/skips/<kind>-<inode>` as the node, and every
node's `scrape_plan` leaves out an item with such a marker unless asked with `--again`
(`All`), so the same question is not asked on every run and from every device; a
renamed item keeps its answer, one deleted and made again is asked anew. An `s`, a run
without a terminal and `--yes` remember nothing; without a
terminal the item is skipped and listed in the summary; `--yes` takes the top result.
`--id` names the item's id outright. A show's match carries its seasons and episodes
(one request per season), matched by number; an episode TMDB does not list is skipped.

**What is written** (I-299, I-300). Only missing slots, and only as new files: each
`PUT` is an exclusive create, so a file that appeared since the plan (another scrape, a
person) answers 412 and is counted as present; a scrape never replaces, renames or
deletes anything, so it runs the same inside a protected folder (13 §4: creating is
allowed there). An `.nfo` is Kodi XML (`movie`, `tvshow` or `episodedetails`) carrying
`title`, `originaltitle`, `year`, `premiered` or `aired`, `plot`, `runtime`, `genre`,
`ratings/rating` (`name="themoviedb"`, `default="true"`), `mpaa` (the `--region`
certification), `director`, up to ten `actor/name`, `season` and `episode`, `status`,
and `uniqueid type="tmdb" default="true"`, so §8's reader takes back every field the
catalogue keeps. `--dry-run` prints the matches and the files it would write and writes
nothing. A run ends by asking the leader for an index pass (`index_now`) and prints how
many files it wrote for how many items, and the items it skipped and why.

**Network** (I-302). The client is the update client's shape (12 §5): one
`http.Client` with a 30 s timeout, `User-Agent: tailraid/<version>
(+https://github.com/zombar/tailraid)`, JSON bodies read through a 4 MiB limit and
images through 20 MiB, a 429 retried after its `Retry-After` at most three times. It
speaks only to `api.themoviedb.org`, `image.tmdb.org`, `musicbrainz.org` and
`coverartarchive.org` (and the `archive.org` hosts the archive redirects an image to) and
`thumbnails.libretro.com`, a redirect anywhere else refused; MusicBrainz is asked at most once a second, as its rules ask;
TMDB with at most four requests in flight. The key goes only to `api.themoviedb.org`
(as `api_key`, or as a bearer token for a v4 token), never to a node, and never into a
log line or an error (a request's URL is printed with the key removed). TMDB's terms
ask for attribution: `media scrape --help` and the README say "This
product uses the TMDB API but is not endorsed or certified by TMDB." MusicBrainz and
the Cover Art Archive data are CC0 and need none.

## Invariants

- **I-298** — No node contacts a metadata provider: only the CLI's `media scrape`
  does, from the device it runs on; no package a node runs imports the
  scrape client.
- **I-299** — A scrape writes a slot only when no name §11 accepts for it exists, only as
  an exclusive create (`If-None-Match: *`; a 412 counts as present), and never replaces,
  renames or deletes a file; `--dry-run` writes nothing.
- **I-300** — What a scrape writes, the catalogue reads back: each written `.nfo` decodes
  (§8) to the provider's title, original title, year, plot, runtime, genres, rating,
  certification, people, season, episode and air date, and each written image is the
  slot's art in the catalogue entry (poster, fanart, thumb, season poster, album cover).
- **I-301** — A clear match (the only result whose tokenized title or original title,
  and year when the name has one, equal the query's; for an album the only release group
  whose artist and title equal the tags'; for a game the set's name equal to its file's,
  else the only one of its title, else the only one of those of its regions) is taken
  without asking; any other result is
  taken only by the user's choice, `--yes` or `--id`, and without a terminal the item is
  skipped and reported; the same answers give the same matches.
- **I-394** — An item answered "no match" is left out of every node's `scrape_plan` until
  asked with `--again`, and only an explicit `n` records it (never an `s`, a run without a
  terminal or `--yes`); the marker is keyed by the item's kind and inode and written only
  by the node, under `/.tailraid/scrape/skips`, after the caller's Write on the item was
  checked.
- **I-395** — Titles compare with every word kept and II..XX read as 2..20, the same on
  both sides; of several equal album titles the one whose year is the tags' is clear;
  a game's short region tags are read as the set's region words.
- **I-302** — The TMDB key lives in the secrets store as `tmdb` (sealed, docs/19, set
  only by `secret set`) and in no file, travels in plaintext only to
  `api.themoviedb.org` and from a node to its own CLI, and never appears in a log line,
  an error or a request between nodes; the client speaks only to the provider hosts (TMDB's two,
  MusicBrainz, the Cover Art Archive and the `archive.org` hosts it redirects to, and
  `thumbnails.libretro.com`), sends
  its user agent, reads bounded bodies, retries a 429 at most three times after its
  `Retry-After`, and asks MusicBrainz at most once a second.
- **I-206** — The index is written only by the leader, only under `/.tailraid/index`,
  only as immutable segments `<seq>.seg` created with `O_EXCL` (a losing create abandons
  the pass), each written to a temp sibling, flushed and renamed, carrying its chain and a
  trailer hash; readers take the highest segment whose trailer verifies and its chain,
  skip one that fails, and reload when the folder's version moved; a pass with nothing
  changed writes nothing; a chain never exceeds `RebuildAt` segments because the pass
  then rebuilds into one; the hourly sweep removes a segment outside the current chain
  once it is an hour old and never touches one inside it.
- **I-207** — A pass indexes files only, keyed by inode, by content: the first
  `search.max_file` bytes of a text file by extension, the first chunk's tags of an audio
  file, nothing of anything else; it skips `/.tailraid` and ephemeral names, and indexes a
  file in the trash like any other (a query never shows one, I-208; a merge drops its doc
  once the file is purged), so a restore needs no pass; it re-indexes a file only when `sha256(Chunks)` changed, so a rename, a move or a
  `SetAttr` costs nothing; it reads at most `search.batch` candidates or `search.batch_bytes`
  of text a pass, `search.workers` at a time at background priority; and above
  `search.max_bytes` of segments it indexes tags, names and the catalogue only and
  `status` says `capped`.
- **I-208** — `GET /api/search` folds the query as it folds the index, ANDs the words with
  the last as a prefix, limits to `under`'s live subtree, answers name hits from the
  FSM's current state and tag and content hits from the segments, never names an entry in
  the trash or under `/.tailraid`, orders name, tags, content, then term frequency, path
  and inode, applies `limit` (50 default, 200 max, 400 outside `1..200`) after the sort,
  and two nodes with the same chain and FSM return byte-identical bodies; a node without
  a loadable chain answers the name hits with `index: unavailable`.
- **I-411** — `ParseQuery` turns a query of at most `QueryMax` bytes and `TermsMax` terms
  into folded words and field terms by §6a's table, the same for every scope; an unknown
  field, an unclosed quote, an empty value, a malformed year or a bound exceeded is a 400
  naming the offset of the fault; it never panics on any input.
- **I-412** — A field term matches the catalogue items whose field holds the value (any of
  its words unquoted, the whole folded value quoted; `actor:` past the entry's
  `directors`, `director:` within them); the field table is a pure function of the
  catalogue, rebuilt when the chain changes and equal to one built from scratch; `scope`
  only chooses whether a hit is an item (grouped by library, sorted as the library) or a
  file of one, so both scopes match the same items, and two nodes with the same chain and
  FSM return byte-identical bodies.
- **I-413** — `GET /api/search/suggest` answers, per field in a fixed order, the values
  whose folded words start with the query's last word and that match at least one item
  with the rest of the query, each with its count by kind, ordered by count then value,
  at most `limit` per field (8 by default, 1..50, else 400); the same on every node.
- **I-210** — `Inode.Flags` bits 2–3 are the library kind (none, movies, tv, music),
  settable on directories only through `SetAttr` with `AttrFlags` at codec 6, refused with
  `ErrCodec` while a live member reads an older codec and with `ErrBadCommand` for any
  other bit; the effective kind is the nearest flagged ancestor's; `ls` and `stat` carry
  `library`, `POST /api/flags` sets it, and `Store.Libraries()` lists flagged folders in
  kind-then-path order, the same on every node.
- **I-384** — `Store.Libraries()` reads an applier-kept set of the directories whose flags
  carry a library kind, never a walk of the inodes: its cost is the libraries' number times
  their depth, and the set equals a recount after every apply and after `Restore`.
- **I-387** — The leader's pass finds its candidates in the change feed and the pending
  set: it walks every inode only when the feed cannot answer and once every `SweepEvery`.
  It rebuilds the catalogue only for the libraries a change lies under. A pass with
  nothing changed under a candidate or a library reads and writes nothing. Over any
  sequence of changes, the chain and catalogue it writes equal those a pass walking
  everything would write.
- **I-388** — A query's name list is brought up to date from the change feed, O(changes
  since the last query), and built by a walk only when the feed cannot answer. Its hits
  equal those of a list built from scratch.
- **I-211** — The catalogue is a pure function of the tree, the sidecars and the tags
  under each library folder: a film from a folder or a loose video file, a show from a
  folder with seasons and episodes parsed from `SnnEnn`, `nnxnn` or `Episode nn`, an album
  as 13 §6 defines it; `.nfo` fields override name-derived ones and a sidecar that is not
  XML contributes nothing; art is resolved in the order poster, `<name>-poster`, folder
  for the poster and fanart, `<name>-fanart` for the background, `<name>-thumb` then the
  browser-made thumbnail for the frame; every node derives the same entries.

- **I-276** — `Analyze` is a pure function of a name and the folder names above it inside
  its library: the fields of §8a's table, and the role of a folder; it never panics, and
  the same input gives the same output on every node; every row of `testdata/names.tsv`
  (real names) gives its expected fields; the catalogue takes every name-derived field
  from it, so a disc folder is part of its album, a category or format folder is walked
  through and an extras folder is never an item.
- **I-277** — `Inode.Flags` bits 2–4 are the library kind; kind 4 is `roms`, settable at
  codec 10 only, refused with `ErrCodec` while a live member reads an older codec and,
  decoded below codec 10, refused as malformed; kinds 0–3 keep their codec 6 encoding.
- **I-278** — Under a `roms` library the catalogue holds one platform per child folder and
  one game per file of an extension that platform takes (or per `cue`/`m3u`/`chd` folder),
  walking region, category and format folders, one game for the discs of one title, and
  nothing for any other file; `gamelist.xml` fields override name-derived ones and one that
  does not parse contributes nothing; no ROM file is read; every node derives the same
  entries.
- **I-400** — A platform's poster is its folder's own art (`platform`, `poster`, `folder`,
  `logo`), else the box art of the first of its games by sort title that has box art, as a
  path below the platform folder; a platform none of whose games has box art has none; every
  node derives the same.

## Tests

| Invariant | Tests |
|---|---|
| I-298 | `TestNoNodePackageImportsScrape` |
| I-299 | `TestScrapePlan_Slots`, `TestScrapePlan_SameOnEveryNode`, `TestPlanPage_Sizes_Table`, `TestPlanPage_Under`, `TestRun_WritesMissingSlotsOnly`, `TestWebWriter_ExclusiveCreate`, `TestCLI_ScrapeWriter_SignsIn` |
| I-300 | `TestNFO_RoundTrip_Table`, `TestScrapePlan_Slots` |
| I-301 | `TestClear_Table`, `TestClearAlbum_Table`, `TestClearGame_Table`, `TestRun_UnclearMatch_Table` |
| I-394 | `TestRun_NoMatchRemembered`, `TestScrapePlan_LeavesOutRememberedNoMatch`, `TestScrapeSkip_NeedsWriteAndIsTheNodes` |
| I-395 | `TestNorm_ShortWordsAndNumerals`, `TestClearAlbum_SelfTitledByYear`, `TestClearGame_ShortRegionTags` |
| I-302 | `TestClient_KeyOnlyToTMDB_UserAgent_Redacted`, `TestClient_ProviderHostsOnly`, `TestClient_429Retries_BoundedBody`, `TestClient_MusicBrainzOncePerSecond` |
| I-206 | `TestSegment_RoundTrip_TrailerRejectsTruncation`, `TestSegment_SizeBoundaries_Table`, `FuzzSegmentDecode`, `TestIndexer_LoserOfCreateRaceAborts`, `TestIndexer_NoChangeWritesNothing`, `TestIndexer_RebuildWhenChainExceedsEight`, `TestSweep_RemovesOrphansOlderThanHour`, `TestSearch_ReloadsOnNewSegment`, `TestCluster_SearchDeterministicAcrossNodes`, `TestIndexer_LeaderChange_ContinuesFromChain`, `TestReport_SearchChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-207 | `TestTokenize_Table`, `FuzzTokenize`, `TestIndexer_SortedWalk_SegmentBytesDeterministic`, `TestIndexer_ReindexOnlyWhenChunksChange`, `TestIndexer_SkipsTrashReservedEphemeral`, `TestIndexer_MaxFile_Table`, `TestIndexer_AudioFirstChunkOnly`, `TestIndexer_MaxBytes_TagsOnly`, `TestConfig_SearchKeys_Table`, `TestReport_SearchChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-208 | `TestSearch_AndPrefix_RankNameTagsContent`, `TestSearch_FiltersDeletedAndUnder`, `TestSearch_LimitBoundaries`, `TestSearch_NameListFollowsApplied`, `TestCluster_SearchDeterministicAcrossNodes`, `TestCLI_Search`, `TestWebUI_Search_Table`, `TestSoak_AllPhasesAgainstHarness` |
| I-387 | `TestPass_FromFeed_MatchesFullWalk`, `TestPass_NothingChanged_WalksNothing`, `TestPass_CatalogueRebuildsTouchedLibraryOnly`, `TestPass_LateTrackRebuildsItsAlbum` |
| I-388 | `TestSearch_Names_FromFeed_MatchesRebuild`, `TestSearch_Names_UpdateCostIsTheChanges` |
| I-384 | `TestStore_Libraries_CostIndependentOfInodes`, `TestStore_Libraries`, `TestFSM_ApplySnapshotRestoreEquivalence` |
| I-411 | `TestParseQuery_Table`, `TestParseQuery_Bounds_Table`, `FuzzParseQuery`, `TestWebUI_Search_MediaScope`, `TestSoak_AllPhasesAgainstHarness` |
| I-412 | `TestFields_Match_Table`, `TestFields_TableFromCatalogue_EqualsRebuild`, `TestSearch_ScopesMatchSameItems`, `TestCatalog_SectionVersions`, `TestCluster_SearchDeterministicAcrossNodes`, `TestWebUI_Search_MediaScope`, `TestSoak_AllPhasesAgainstHarness`, `TestReport_MediaQueries` |
| I-413 | `TestSuggest_Table`, `TestSuggest_LimitBoundaries`, `TestCluster_SearchDeterministicAcrossNodes`, `TestWebUI_Search_Suggest`, `TestSoak_AllPhasesAgainstHarness` |
| I-210 | `TestFSM_SetAttr_LibraryKind_Table`, `TestFSM_LibraryEffective_NearestAncestor`, `TestStore_Libraries`, `TestEncodeCommand_V6Gating`, `TestEncodeCommand_Golden`, `TestCluster_Codec6RefusedWhileMemberOld`, `TestWebUI_Flags_Library_Table` |
| I-276 | `TestNames_Table`, `FuzzAnalyze`, `TestNames_Corpus`, `TestNames_FolderRoles`, `TestCatalog_MusicRoles_Table`, `TestCatalog_ExtrasNotItems`, `TestCatalog_SectionVersions` |
| I-277 | `TestFSM_SetAttr_LibraryRoms_Table`, `TestEncodeCommand_V10Gating`, `TestCluster_Codec10RefusedWhileMemberOld`, `TestWebUI_Flags_Library_Table`, `TestFS_SetFlags_RomsWaitsForCodec10` |
| I-278 | `TestCatalog_Roms_Table`, `TestCatalog_Gamelist_Table`, `FuzzGamelist`, `TestCatalog_SectionVersions` |
| I-400 | `TestCatalog_Roms_PlatformArt`, `TestWebUI_Media_PlatformArt` |
| I-211 | `TestCatalog_CleanName_Table`, `TestCatalog_Episode_Table`, `TestCatalog_NFO_Table`, `FuzzNFODecode`, `TestCatalog_ArtResolutionOrder`, `TestCatalog_LibraryKinds_Structure`, `TestCluster_SearchDeterministicAcrossNodes`, `TestSoak_AllPhasesAgainstHarness` |

Soak phase 21 joins these rows with the PR that lands it (09 §2). The web side's invariants (I-209 markdown,
I-212 the Media tab) are in 13.
