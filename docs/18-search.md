# 18 — Search, the index in the volume and the media catalogue

One search box finds files and folders by name across the volume, and text files and
audio tracks by what is in them; a Media tab (13 §10) browses films, shows and music by
poster and cover with the descriptions and art the volume holds. Names need no index:
every node has the whole tree in memory. Contents and media metadata are read once, by
the leader, into an **index kept inside the volume** under `/.tailraid/index`, so every
node reads the same index as it reads any other file, and a device that joins tomorrow
has it at once. This reverses the "search" non-goal 13 §9 carried until now.

*Status (2026-10-11): designed; docs, invariants, lookdev and stubs first. I-206..I-212
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
  `fanart.*`), embedded tags and the names themselves. No metadata is fetched from the
  internet: a node never talks to a provider, and an item without sidecars shows its
  cleaned name and the browser-made thumbnail (13 §5).
- Everything is written by the leader only, as immutable files, and read by any node.

## 2. What is indexed (I-207)

Files only, keyed by inode (`Ino` is never reused, 00 §Glossary, I-3), so a rename or a
move needs no re-index and a file is re-indexed only when its content changed: the doc
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
the folder. The rules:

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
(10 min, `"0"` off) when `Applied` advanced since the last pass: load the chain, walk
every inode once (`Store.ForEachInode`, the `ForEachChunk` pattern: keys sorted, copies
taken in batches of 1000 under the read lock, so a million inodes never starve Apply),
collect the candidates in inode order, take at most `search.batch` of them or
`search.batch_bytes` of text, read them `search.workers` (2) at a time under
`chunk.Background` (06 §4, I-175), tokenize, derive
the catalogue entries (§8), write the segment (§4). While candidates are pending the
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
[{term, off}]}` with `match` one of `name`, `tags`, `content`:

- **Names** are matched live against the FSM, so a file written a second ago is found by
  its name at once. The node keeps a folded name list (every live inode's words) rebuilt
  when `Applied` moved and at least `NameRefresh` (1 s) has passed since the last
  build, and reused otherwise: never a full walk per keystroke, even while a copy keeps
  the FSM moving.
- **Tags** and **content** come from the segments. Every candidate inode is filtered
  through the live `GetInode` (nothing in the trash or under `/.tailraid` is a hit) and
  `PathOf` names it.
- **Order**: name hits, then tag hits, then content hits; within a group by the sum of
  term frequencies descending, then path ascending, then inode; `limit` applies after the
  whole sort. **Determinism**: two nodes with the same chain and the same FSM return
  byte-identical bodies (the N-node test).
- A node whose chain fails to load answers the name hits with `"index": "unavailable"`.

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
  genres, rating, runtime, mpaa, people, season, episode, aired, poster, fanart, thumb`,
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
its discs. Region and revision variants of one title are separate entries that the Media
tab shows as one card. Fields come from the name (§8a: title, regions, languages,
revision, flags, disc) and, when the platform folder holds one, an EmulationStation
`gamelist.xml` (`game/path`, `name`, `desc`, `releasedate`, `developer`, `publisher`,
`genre`, `players`, `rating`, `image`, `thumbnail`, `marquee`), read like an `.nfo` (the
first `search.max_file` bytes, `encoding/xml`, nothing when it does not parse). Box art is
`gamelist.xml`'s image, else `<stem>.png|jpg` beside the game, `images/<stem>*`,
`media/boxart/<stem>.*`, then `boxart.*` or `cover.*` in a game folder; else the Media tab
draws a title card (13 §5). The ROM file is never read: no header parse, no hash, no
DAT match.

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
folder in kind-then-path order for the Media tab; no new CLI command is needed.

## 10. Non-goals

Stemming, fuzzy matching, regular expressions, PDF or Office text, EXIF, content past
`search.max_file`, a shared query history, online metadata providers (TMDB, TVDB,
MusicBrainz, IGDB, ScreenScraper), subtitles, watched state, transcoding, a music artist
page beyond the biography and art, launching or emulating a game, reading a ROM's header
and matching ROMs against DAT files.

## Invariants

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
  file, nothing of anything else; it skips `/.tailraid`, deleted roots and ephemeral
  names; it re-indexes a file only when `sha256(Chunks)` changed, so a rename, a move or a
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
- **I-210** — `Inode.Flags` bits 2–3 are the library kind (none, movies, tv, music),
  settable on directories only through `SetAttr` with `AttrFlags` at codec 6, refused with
  `ErrCodec` while a live member reads an older codec and with `ErrBadCommand` for any
  other bit; the effective kind is the nearest flagged ancestor's; `ls` and `stat` carry
  `library`, `POST /api/flags` sets it, and `Store.Libraries()` lists flagged folders in
  kind-then-path order, the same on every node.
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

## Tests

| Invariant | Tests |
|---|---|
| I-206 | `TestSegment_RoundTrip_TrailerRejectsTruncation`, `TestSegment_SizeBoundaries_Table`, `FuzzSegmentDecode`, `TestIndexer_LoserOfCreateRaceAborts`, `TestIndexer_NoChangeWritesNothing`, `TestIndexer_RebuildWhenChainExceedsEight`, `TestSweep_RemovesOrphansOlderThanHour`, `TestSearch_ReloadsOnNewSegment`, `TestCluster_SearchDeterministicAcrossNodes`, `TestIndexer_LeaderChange_ContinuesFromChain`, `TestReport_SearchChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-207 | `TestTokenize_Table`, `FuzzTokenize`, `TestIndexer_SortedWalk_SegmentBytesDeterministic`, `TestIndexer_ReindexOnlyWhenChunksChange`, `TestIndexer_SkipsTrashReservedEphemeral`, `TestIndexer_MaxFile_Table`, `TestIndexer_AudioFirstChunkOnly`, `TestIndexer_MaxBytes_TagsOnly`, `TestConfig_SearchKeys_Table`, `TestReport_SearchChart`, `TestSoak_AllPhasesAgainstHarness` |
| I-208 | `TestSearch_AndPrefix_RankNameTagsContent`, `TestSearch_FiltersDeletedAndUnder`, `TestSearch_LimitBoundaries`, `TestSearch_NameListFollowsApplied`, `TestCluster_SearchDeterministicAcrossNodes`, `TestCLI_Search`, `TestWebUI_Search_Table`, `TestSoak_AllPhasesAgainstHarness` |
| I-210 | `TestFSM_SetAttr_LibraryKind_Table`, `TestFSM_LibraryEffective_NearestAncestor`, `TestStore_Libraries`, `TestEncodeCommand_V6Gating`, `TestEncodeCommand_Golden`, `TestCluster_Codec6RefusedWhileMemberOld`, `TestWebUI_Flags_Library_Table` |
| I-276 | `TestNames_Table`, `FuzzAnalyze`, `TestNames_Corpus`, `TestNames_FolderRoles`, `TestCatalog_MusicRoles_Table`, `TestCatalog_ExtrasNotItems`, `TestCatalog_SectionVersions` |
| I-277 | `TestFSM_SetAttr_LibraryRoms_Table`, `TestEncodeCommand_V10Gating`, `TestCluster_Codec10RefusedWhileMemberOld`, `TestWebUI_Flags_Library_Table`, `TestFS_SetFlags_RomsWaitsForCodec10` |
| I-278 | `TestStub_I_278` (planned: TestCatalog_Roms_Table, TestCatalog_Gamelist_Table, FuzzGamelist, TestCluster_SearchDeterministicAcrossNodes) |
| I-211 | `TestCatalog_CleanName_Table`, `TestCatalog_Episode_Table`, `TestCatalog_NFO_Table`, `FuzzNFODecode`, `TestCatalog_ArtResolutionOrder`, `TestCatalog_LibraryKinds_Structure`, `TestCluster_SearchDeterministicAcrossNodes`, `TestSoak_AllPhasesAgainstHarness` |

Soak phase 21 joins these rows with the PR that lands it (09 §2). The web side's invariants (I-209 markdown,
I-212 the Media tab) are in 13.
