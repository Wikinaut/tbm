package scraper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	FetchInterval = 1 * time.Minute
)

type Scraper struct {
	AccessToken string `json:"access_token"`
	csrfToken   string
	Cookie      string `json:"cookie"`
	Section     string `json:"section"`
	variables   map[string]interface{}
	features    map[string]interface{}

	mx         sync.RWMutex
	close      chan bool
	OnNewTweet func(ct *CachedTweet) bool `json:"-"`

	Delay       time.Duration `json:"-"`
	Timeout     time.Duration `json:"-"`
	lastRequest time.Time

	RawTimeout string `json:"timeout"`
	RawDelay   string `json:"delay"`
}

func NewScraper() *Scraper {
	return &Scraper{
		AccessToken: "",
		csrfToken:   "",
		Cookie:      "",
		Section:     "BvX-1Exs_MDBeKAedv2T_w",
		Delay:       time.Second * 30,
		Timeout:     time.Second * 10,
		lastRequest: time.Time{},
		variables: map[string]interface{}{
			"count":                       20,
			"cursor":                      "",
			"includePromotedContent":      true,
			"withSuperFollowsUserFields":  true,
			"withDownvotePerspective":     false,
			"withReactionsMetadata":       false,
			"withReactionsPerspective":    false,
			"withSuperFollowsTweetFields": true,
		},
		features: map[string]interface{}{
			"dont_mention_me_view_api_enabled":      true,
			"interactive_text_enabled":              true,
			"responsive_web_uc_gql_enabled":         true,
			"vibe_api_enabled":                      true,
			"responsive_web_edit_tweet_api_enabled": false,
			"standardized_nudges_misinfo":           true,
			"tweet_with_visibility_results_prefer_gql_limited_actions_policy_enabled": false,
			"responsive_web_enhance_cards_enabled":                                    false,
		},
	}
}

func (s *Scraper) Start() {
	s.LoadCsrfToken()
	go s.run()
	s.close = make(chan bool)

	ticker := time.NewTicker(FetchInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.run()
			case <-s.close:
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *Scraper) Stop() {
	s.close <- true
}

func (s *Scraper) delayRequest() {
	delta := s.Delay - time.Now().Sub(s.lastRequest)
	if delta > 0 {
		time.Sleep(delta)
	}
}

func (s *Scraper) LoadCsrfToken() bool {
	for _, p := range strings.Split(s.Cookie, ";") {
		parts := strings.SplitN(p, "=", 2)
		if strings.Trim(parts[0], " ") == "ct0" {
			s.csrfToken = strings.Trim(parts[1], " ")
			return true
		}
	}
	return false
}

func (s *Scraper) SetAccessTokens(AccessToken, Cookie string) bool {
	s.mx.Lock()
	defer s.mx.Unlock()

	s.AccessToken = AccessToken
	s.Cookie = Cookie
	return s.LoadCsrfToken()
}

func (s *Scraper) buildUrl() string {
	jvb, _ := json.Marshal(s.variables)
	fvb, _ := json.Marshal(s.features)

	return fmt.Sprintf(
		"https://twitter.com/i/api/graphql/%s/Bookmarks?variables=%s&features=%s",
		s.Section,
		url.QueryEscape(string(jvb)),
		url.QueryEscape(string(fvb)),
	)
}

func (s *Scraper) run() {
	s.mx.Lock()
	defer s.mx.Unlock()

	req, err := http.NewRequest("GET", s.buildUrl(), nil)
	if err != nil {
		fmt.Printf("client: error making http request: %s\n", err)
		return
	}
	req.Header.Set("Cookie", s.Cookie)
	req.Header.Set("authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-csrf-token", s.csrfToken)

	s.delayRequest()
	res, err := http.DefaultClient.Do(req)
	s.lastRequest = time.Now()

	if err != nil {
		fmt.Printf("client: error making http request: %s\n", err)
		return
	}

	if res.StatusCode != 200 {
		fmt.Printf("twitter: failed to fetch response body: %d\n", res.StatusCode)
		return
	}

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		fmt.Printf("client: could not read response body: %s\n", err)
		return
	}

	rb := &BookmarkResponse{}
	if err := json.Unmarshal(resBody, rb); err != nil {
		fmt.Printf("twitter: could not read response body: %s\n", err)
		return
	}

	cursor := ""
	for _, instruction := range rb.Data.BookmarkTimeline.Timeline.Instructions {
		for _, entry := range instruction.Entries {
			switch entry.Content.EntryType {
			case "TimelineTimelineItem":
				// Tweet
				if s.OnNewTweet(&CachedTweet{
					User:  entry.Content.ItemContent.TweetResults.Result.Core.UserResults.Result,
					Tweet: entry.Content.ItemContent.TweetResults.Result.Legacy,
				}) == false {
					return
				}
			case "TimelineTimelineCursor":
				//Cursor
				if entry.Content.CursorType == "Bottom" {
					cursor = entry.Content.Value
				}
			}
		}
	}

	if cursor != "" {
		s.variables["cursor"] = cursor
		go s.run()
	}
}

func (s *Scraper) Download(src, target string) error {
	b, err := s.Get(src)
	if err != nil {
		return err
	}

	return os.WriteFile(target, b, 0644)
}

func (s *Scraper) TweetDetail(id string) (*ConversationResponse, error) {
	req, err := http.NewRequest("GET", "https://twitter.com/i/api/2/timeline/conversation/"+id+".json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", s.Cookie)
	req.Header.Set("authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-csrf-token", s.csrfToken)

	q := req.URL.Query()
	q.Add("include_profile_interstitial_type", "1")
	q.Add("include_blocking", "1")
	q.Add("include_blocked_by", "1")
	q.Add("include_followed_by", "1")
	q.Add("include_want_retweets", "1")
	q.Add("include_mute_edge", "1")
	q.Add("include_can_dm", "1")
	q.Add("include_can_media_tag", "1")
	q.Add("include_ext_has_nft_avatar", "1")
	q.Add("skip_status", "1")
	q.Add("cards_platform", "Web-12")
	q.Add("include_cards", "1")
	q.Add("include_ext_alt_text", "true")
	q.Add("include_quote_count", "true")
	q.Add("include_reply_count", "1")
	q.Add("tweet_mode", "extended")
	q.Add("include_entities", "true")
	q.Add("include_user_entities", "true")
	q.Add("include_ext_media_color", "true")
	q.Add("include_ext_media_availability", "true")
	q.Add("include_ext_sensitive_media_warning", "true")
	q.Add("send_error_codes", "true")
	q.Add("simple_quoted_tweet", "true")
	q.Add("include_tweet_replies", "true")
	q.Add("ext", "mediaStats,highlightedLabel,hasNftAvatar,voiceInfo,superFollowMetadata")
	req.URL.RawQuery = q.Encode()

	s.delayRequest()
	resp, err := http.DefaultClient.Do(req)
	s.lastRequest = time.Now()

	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("failed to download resource with \"" + resp.Status + "\" from " + "https://twitter.com/i/api/2/timeline/conversation/" + id + ".json")
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	v := &ConversationResponse{}
	if err := json.Unmarshal(b, v); err != nil {
		fmt.Printf("twitter: could not read response body: %s\n", err)
		return nil, err
	}
	return v, nil
}

func (s *Scraper) Get(src string) ([]byte, error) {
	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", s.Cookie)
	req.Header.Set("authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-csrf-token", s.csrfToken)

	s.delayRequest()
	resp, err := http.DefaultClient.Do(req)
	s.lastRequest = time.Now()

	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("failed to download resource with \"" + resp.Status + "\" from " + src)
	}

	return io.ReadAll(resp.Body)
}