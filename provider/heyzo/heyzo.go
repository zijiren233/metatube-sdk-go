package heyzo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/gocolly/colly/v2"
	"github.com/grafov/m3u8"
	"github.com/javtube/javtube-sdk-go/common/parser"
	"github.com/javtube/javtube-sdk-go/model"
	"github.com/javtube/javtube-sdk-go/provider"
)

var _ provider.Provider = (*Heyzo)(nil)

const (
	baseURL   = "https://www.heyzo.com/"
	movieURL  = "https://www.heyzo.com/moviepages/%04s/index.html"
	sampleURL = "https://www.heyzo.com/contents/%s/%s/%s"
)

type Heyzo struct {
	c *colly.Collector
}

func NewHeyzo() *Heyzo {
	return &Heyzo{
		c: colly.NewCollector(colly.UserAgent(provider.UA)),
	}
}

func (hzo *Heyzo) Name() string {
	return "HEYZO"
}

func (hzo *Heyzo) GetMovieInfoByID(id string) (info *model.MovieInfo, err error) {
	return hzo.GetMovieInfoByLink(fmt.Sprintf(movieURL, id))
}

func (hzo *Heyzo) GetMovieInfoByLink(link string) (info *model.MovieInfo, err error) {
	homepage, err := url.Parse(strings.TrimRight(link, "/"))
	if err != nil {
		return nil, err
	}
	id := path.Base(path.Dir(homepage.Path))

	info = &model.MovieInfo{
		ID:            id,
		Number:        fmt.Sprintf("HEYZO-%s", id),
		Homepage:      homepage.String(),
		Maker:         "HEYZO",
		Actors:        []string{},
		PreviewImages: []string{},
		Tags:          []string{},
	}

	c := hzo.c.Clone()

	// JSON
	c.OnXML(`//script[@type="application/ld+json"]`, func(e *colly.XMLElement) {
		data := struct {
			Name          string `json:"name"`
			Image         string `json:"image"`
			Description   string `json:"description"`
			ReleasedEvent struct {
				StartDate string `json:"startDate"`
			} `json:"releasedEvent"`
			Video struct {
				Duration string `json:"duration"`
				Actor    string `json:"actor"`
				Provider string `json:"provider"`
			} `json:"video"`
			AggregateRating struct {
				RatingValue string `json:"ratingValue"`
			} `json:"aggregateRating"`
		}{}
		if json.Unmarshal([]byte(e.Text), &data) == nil {
			info.Title = data.Name
			info.Summary = data.Description
			info.CoverURL = e.Request.AbsoluteURL(data.Image)
			info.ThumbURL = info.CoverURL /* use cover as thumb */
			info.Publisher = data.Video.Provider
			info.ReleaseDate = parser.ParseDate(data.ReleasedEvent.StartDate)
			info.Duration = parser.ParseDuration(data.Video.Duration)
			info.Score = parser.ParseScore(data.AggregateRating.RatingValue)
			if data.Video.Actor != "" {
				info.Actors = []string{data.Video.Actor}
			}
		}
	})

	// Title
	c.OnXML(`//*[@id="movie"]/h1`, func(e *colly.XMLElement) {
		if info.Title == "" {
			info.Title = strings.Fields(e.Text)[0]
		}
	})

	// Summary
	c.OnXML(`//p[@class="memo"]`, func(e *colly.XMLElement) {
		if info.Summary == "" {
			info.Summary = strings.TrimSpace(e.Text)
		}
	})

	// Thumb+Cover (fallback)
	c.OnXML(`//meta[@property="og:image"]`, func(e *colly.XMLElement) {
		if info.CoverURL == "" {
			info.CoverURL = e.Request.AbsoluteURL(e.Attr("content"))
			info.ThumbURL = info.CoverURL
		}
	})

	// Fields
	c.OnXML(`//table[@class="movieInfo"]/tbody/tr`, func(e *colly.XMLElement) {
		switch e.ChildText(`.//td[1]`) {
		case "公開日":
			info.ReleaseDate = parser.ParseDate(e.ChildText(`.//td[2]`))
		case "出演":
			info.Actors = e.ChildTexts(`.//td[2]/a/span`)
		case "シリーズ":
			info.Series = strings.Trim(e.ChildText(`.//td[2]`), "-")
		case "評価":
			info.Score = parser.ParseScore(e.ChildText(`.//span[@itemprop="ratingValue"]`))
		}
	})

	// Tags
	c.OnXML(`//ul[@class="tag-keyword-list"]`, func(e *colly.XMLElement) {
		info.Tags = e.ChildTexts(`.//li/a`)
	})

	// Video+Duration
	c.OnXML(`//script[@type="text/javascript"]`, func(e *colly.XMLElement) {
		// Sample Video
		if strings.Contains(e.Text, "emvideo") {
			if sub := regexp.MustCompile(`emvideo = "(.+?)";`).FindStringSubmatch(e.Text); len(sub) == 2 {
				info.PreviewVideoURL = e.Request.AbsoluteURL(sub[1])
			}
		}
		// Duration
		if strings.Contains(e.Text, "o = {") {
			if sub := regexp.MustCompile(`o = (\{.+?});`).FindStringSubmatch(e.Text); len(sub) == 2 {
				data := struct {
					Full string `json:"full"`
				}{}
				if json.Unmarshal([]byte(sub[1]), &data) == nil {
					info.Duration = parser.ParseDuration(data.Full)
				}
			}
		}
	})

	// Preview Video
	c.OnXML(`//*[@id="playerContainer"]/script`, func(e *colly.XMLElement) {
		if !strings.Contains(e.Text, "movieId") {
			return
		}
		var movieID, siteID string
		if sub := regexp.MustCompile(`movieId\s*=\s*'(\d+?)';`).FindStringSubmatch(e.Text); len(sub) == 2 {
			movieID = sub[1]
		}
		if sub := regexp.MustCompile(`siteID\s*=\s*'(\d+?)';`).FindStringSubmatch(e.Text); len(sub) == 2 {
			siteID = sub[1]
		}
		if movieID == "" || siteID == "" {
			return
		}
		if sub := regexp.MustCompile(`stream\s*=\s*'(.+?)'\+siteID\+'(.+?)'\+movieId\+'(.+?)';`).
			FindStringSubmatch(e.Text); len(sub) == 4 {
			d := c.Clone()
			d.OnResponse(func(r *colly.Response) {
				playList, ListType, err := m3u8.Decode(*bytes.NewBuffer(r.Body), true)
				if err == nil && ListType == m3u8.MASTER {
					masterPL := playList.(*m3u8.MasterPlaylist)
					if len(masterPL.Variants) < 1 {
						return
					}
					sort.SliceStable(masterPL.Variants, func(i, j int) bool {
						return masterPL.Variants[i].Bandwidth < masterPL.Variants[j].Bandwidth
					})
					uri := masterPL.Variants[len(masterPL.Variants)-1].URI
					if ss := regexp.MustCompile(`/sample/(\d+)/(\d+)/ts\.(.+?)\.m3u8`).
						FindStringSubmatch(uri); len(ss) == 4 {
						info.PreviewVideoURL = fmt.Sprintf(sampleURL, ss[1], ss[2], ss[3])
					}
				}
			})
			m3u8Link := e.Request.AbsoluteURL(fmt.Sprintf("%s%s%s%s%s", sub[1], siteID, sub[2], movieID, sub[3]))
			d.Visit(m3u8Link)
		}
	})

	// Preview Images
	c.OnXML(`//div[@class="sample-images yoxview"]/script`, func(e *colly.XMLElement) {
		for _, sub := range regexp.MustCompile(`"(/contents/.+/\d+?\.\w+?)"`).FindAllStringSubmatch(e.Text, -1) {
			info.PreviewImages = append(info.PreviewImages, e.Request.AbsoluteURL(sub[1]))
		}
	})

	err = c.Visit(info.Homepage)
	return
}

func (hzo *Heyzo) SearchMovie(keyword string) (results []*model.SearchResult, err error) {
	return nil, provider.ErrNotSupported
}