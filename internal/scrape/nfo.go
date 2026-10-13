package scrape

import (
	"bytes"
	"encoding/xml"
	"strconv"
)

// nfoDoc is the Kodi sidecar a scrape writes (docs/18 §11): every field §8's reader takes
// back, and the TMDB id as the default uniqueid.
type nfoDoc struct {
	XMLName   xml.Name
	Title     string     `xml:"title"`
	Original  string     `xml:"originaltitle,omitempty"`
	Year      string     `xml:"year,omitempty"`
	Premiered string     `xml:"premiered,omitempty"`
	Aired     string     `xml:"aired,omitempty"`
	Plot      string     `xml:"plot,omitempty"`
	Runtime   string     `xml:"runtime,omitempty"`
	Genres    []string   `xml:"genre"`
	Ratings   *nfoRating `xml:"ratings,omitempty"`
	MPAA      string     `xml:"mpaa,omitempty"`
	Directors []string   `xml:"director"`
	Actors    []nfoActor `xml:"actor"`
	Season    string     `xml:"season,omitempty"`
	Episode   string     `xml:"episode,omitempty"`
	Status    string     `xml:"status,omitempty"`
	UniqueID  *nfoID     `xml:"uniqueid,omitempty"`
}

type nfoRating struct {
	Rating struct {
		Name    string `xml:"name,attr"`
		Max     string `xml:"max,attr"`
		Default string `xml:"default,attr"`
		Value   string `xml:"value"`
	} `xml:"rating"`
}

type nfoActor struct {
	Name  string `xml:"name"`
	Order int    `xml:"order"`
}

type nfoID struct {
	Type    string `xml:"type,attr"`
	Default string `xml:"default,attr"`
	ID      string `xml:",chardata"`
}

// NFO is m as a Kodi sidecar (movie, tvshow or episodedetails).
func NFO(m Meta) []byte {
	d := nfoDoc{
		XMLName: xml.Name{Local: m.Root}, Title: m.Title, Premiered: m.Premiered, Aired: m.Aired, Plot: m.Plot, Genres: m.Genres,
		MPAA: m.MPAA, Directors: m.Directors, Status: m.Status,
	}
	if m.Original != "" && m.Original != m.Title {
		d.Original = m.Original
	}
	if m.Year != 0 {
		d.Year = strconv.Itoa(m.Year)
	}
	if m.Runtime != 0 {
		d.Runtime = strconv.Itoa(m.Runtime)
	}
	if m.Rating > 0 {
		r := &nfoRating{}
		r.Rating.Name, r.Rating.Max, r.Rating.Default = "themoviedb", "10", "true"
		r.Rating.Value = strconv.FormatFloat(m.Rating, 'f', 1, 64)
		d.Ratings = r
	}
	for i, a := range m.Actors {
		d.Actors = append(d.Actors, nfoActor{Name: a, Order: i})
	}
	if m.Season >= 0 {
		d.Season = strconv.Itoa(m.Season)
	}
	if m.Episode >= 0 {
		d.Episode = strconv.Itoa(m.Episode)
	}
	if m.TMDB != 0 {
		d.UniqueID = &nfoID{Type: "tmdb", Default: "true", ID: strconv.Itoa(m.TMDB)}
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	_ = enc.Encode(d) // a struct of strings always encodes
	b.WriteByte('\n')
	return b.Bytes()
}
