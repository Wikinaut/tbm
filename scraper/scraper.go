package scraper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"tbm/utils/log"
	"time"
)

const (
	FetchInterval = 1 * time.Minute
)

type Scraper struct {
	AccessToken string `json:"access_token"`
	csrfToken   string
	Cookie      string   `json:"cookie"`
	Sections    Sections `json:"sections"`
	variables   map[string]interface{}
	features    map[string]interface{}

	mx         sync.RWMutex
	close      chan bool
	running    bool
	OnNewTweet func(ct *CachedTweet) bool `json:"-"`

	Delay       time.Duration `json:"-"`
	Timeout     time.Duration `json:"-"`
	lastRequest time.Time

	RawTimeout string `json:"timeout"`
	RawDelay   string `json:"delay"`
}

type Sections struct {
	Index  string `json:"index"`
	Remove string `json:"remove"`
}

func NewScraper() *Scraper {
	return &Scraper{
		AccessToken: "",
		csrfToken:   "",
		Cookie:      "",
		running:     false,
		Sections: Sections{
			Index:  "",
			Remove: "",
		},
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

			"graphql_timeline_v2_bookmark_timeline":                                  false,
			"responsive_web_twitter_blue_verified_badge_is_enabled":                  true,
			"verified_phone_label_enabled":                                           false,
			"responsive_web_graphql_timeline_navigation_enabled":                     true,
			"unified_cards_ad_metadata_container_dynamic_card_content_query_enabled": true,
			"tweetypie_unmention_optimization_enabled":                               true,
			"graphql_is_translatable_rweb_tweet_is_translatable_enabled":             true,
			"responsive_web_text_conversations_enabled":                              false,
		},
	}
}

func (s *Scraper) Start(removeBookmarks bool) {
	s.LoadCsrfToken()
	if err := s.LoadSections(); err != nil {
		log.Error(err)
		return
	}

	go s.Run(removeBookmarks)
	s.close = make(chan bool)

	ticker := time.NewTicker(FetchInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.Run(removeBookmarks)
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
	if s.lastRequest.IsZero() {
		return
	}
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

func (s *Scraper) LoadSections() error {
	src := "https://twitter.com/i/bookmarks"
	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", s.Cookie)

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("failed to download resource with \"" + resp.Status + "\" from " + src)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	re := regexp.MustCompile(`https://abs\.twimg\.com/responsive-web/client-web-legacy/main\.([0-9a-zA-Z]*)\.js`)
	matches := re.FindStringSubmatch(string(b))
	if len(matches) > 0 {
		mainJsFile := matches[0]

		req, err = http.NewRequest("GET", mainJsFile, nil)
		if err != nil {
			return err
		}

		resp, err = http.DefaultClient.Do(req)

		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return errors.New("failed to download resource with \"" + resp.Status + "\" from " + mainJsFile)
		}

		b, err = io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		jsContent := string(b)

		re = regexp.MustCompile(`AAAAAAAAAAAAAAA([a-zA-Z0-9-_%]*)`)
		matches = re.FindStringSubmatch(jsContent)
		if len(matches) > 1 {
			s.AccessToken = matches[0]
		} else {
			return errors.New("failed to locate the access token")
		}

		re = regexp.MustCompile(`"([a-zA-Z0-9-_]*)",operationName:"Bookmarks"`)
		matches = re.FindStringSubmatch(jsContent)
		if len(matches) > 1 {
			s.Sections.Index = matches[1]

			re = regexp.MustCompile(`"([a-zA-Z0-9-_]*)",operationName:"DeleteBookmark"`)
			matches = re.FindStringSubmatch(jsContent)

			if len(matches) > 1 {
				s.Sections.Remove = matches[1]
			} else {
				return errors.New("failed to locate bookmark remove section")
			}
		} else {
			return errors.New("failed to locate bookmark index section")
		}
	} else {
		return errors.New("failed to locate main js file")
	}

	return nil
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
		s.Sections.Index,
		url.QueryEscape(string(jvb)),
		url.QueryEscape(string(fvb)),
	)
}

func (s *Scraper) Run(keepCursor bool) {
	s.mx.Lock()
	defer s.mx.Unlock()
	if s.running {
		return
	}
	s.running = true
	s.run(keepCursor)
}

func (s *Scraper) run(keepCursor bool, attempts ...error) {
	if len(attempts) > 10 {
		log.Error("Api failed to many times: %s", attempts[len(attempts)-1].Error())
		s.free()
		return
	}

	req, err := http.NewRequest("GET", s.buildUrl(), nil)
	if err != nil {
		log.Error("client: error making http request: %s", err.Error())
		s.free()
		return
	}
	req.Header.Set("Cookie", s.Cookie)
	req.Header.Set("authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-csrf-token", s.csrfToken)

	s.delayRequest()
	res, err := http.DefaultClient.Do(req)
	s.lastRequest = time.Now()

	if err != nil {
		log.Error("client: error sending http request: %s", err.Error())
		s.free()
		return
	}

	if res.StatusCode != 200 {
		log.Error("twitter: failed to fetch response body: %s", res.Status)
		s.free()
		return
	}

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Error("client: could not read response body: %s", err)
		s.free()
		return
	}

	rb := &BookmarkResponse{}
	if err := json.Unmarshal(resBody, rb); err != nil {
		log.Error("client: could not unmarshal response body: %s", err)
		s.free()
		return
	}

	if len(rb.Errors) > 0 {
		err := rb.Errors[0]
		log.Warning("twitter: api error at cursor \"%s\" with %s", s.variables["cursor"], err.Message)

		attempts = append(attempts, errors.New(err.Message))
		go s.run(keepCursor, attempts...)
		return
	}

	cursor := ""
	count := 0
	empty := 0
	for _, instruction := range rb.Data.BookmarkTimeline.Timeline.Instructions {
		for _, entry := range instruction.Entries {
			switch entry.Content.EntryType {
			case "TimelineTimelineItem":
				// Tweet
				tweet := entry.Content.ItemContent.TweetResults.Result.Legacy
				user := entry.Content.ItemContent.TweetResults.Result.Core.UserResults.Result
				if tweet.IdStr == "" {
					tweet = entry.Content.ItemContent.TweetResults.Result.Tweet.Legacy
					user = entry.Content.ItemContent.TweetResults.Result.Tweet.Core.UserResults.Result
				}

				if tweet.IdStr == "" {
					log.Info("Empty tweet id. Probably got deleted at some point")
					// @TODO: might want to call
					// 		  s.DeleteBookmarkDetail(entry.Content.ItemContent.TweetResults.Result.RestId)
					//		  to delete this bookmark - but it might also be a twitter issue and the tweet becomes
					//		  available at a later point. I'm assuming RestId equals IdStr, but I could be wrong..
					empty++
				} else {
					if s.OnNewTweet(&CachedTweet{
						User:  user,
						Tweet: tweet,
					}) == false {
						go s.run(keepCursor, attempts...)
						return
					}
				}
				count++
			case "TimelineTimelineCursor":
				//Cursor
				if entry.Content.CursorType == "Bottom" {
					cursor = entry.Content.Value
				}
			}
		}
	}

	if keepCursor {
		if c, ok := s.variables["count"].(int); ok && c <= count {
			if count == empty {
				s.variables["cursor"] = cursor
			}
			go s.run(keepCursor)
		} else {
			s.free()
		}
	} else if cursor != "" {
		s.variables["cursor"] = cursor
		go s.run(keepCursor)
	} else {
		s.free()
	}
}

func (s *Scraper) free() {
	go func() {
		s.mx.Lock()
		defer s.mx.Unlock()
		s.running = false
	}()
}

func (s *Scraper) Download(src, target string) error {
	b, err := s.Get(src)
	if err != nil {
		log.Error("twitter: could not read response body: %s", err)
		return err
	}

	return os.WriteFile(target, b, 0644)
}

type RemoveBookmarkResponse struct {
	Data struct {
		TweetBookmarkDelete string `json:"tweet_bookmark_delete"`
	} `json:"data"`
}

func (s *Scraper) DeleteBookmarkDetail(id string) (*RemoveBookmarkResponse, error) {
	b, err := json.Marshal(map[string]interface{}{
		"variables": map[string]string{
			"tweet_id": id,
		},
		"queryId": s.Sections.Remove,
	})

	req, err := http.NewRequest("POST", "https://twitter.com/i/api/graphql/"+s.Sections.Remove+"/DeleteBookmark", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", s.Cookie)
	req.Header.Set("authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-csrf-token", s.csrfToken)
	req.Header.Set("content-type", "application/json")

	s.delayRequest()
	resp, err := http.DefaultClient.Do(req)
	s.lastRequest = time.Now()

	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("failed to remove bookmark " + id + " with status \"" + resp.Status)
	}

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	v := &RemoveBookmarkResponse{}
	if err := json.Unmarshal(rb, v); err != nil {
		return nil, err
	}

	return v, nil
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

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("failed to download resource with \"" + resp.Status + "\" from " + src)
	}

	return io.ReadAll(resp.Body)
}
