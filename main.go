package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	postRegexp   = regexp.MustCompile(`<li id="s-([0-9]{7})">[A-Za-z0-9 <>="\-\n]*<a href="/users/([0-9]{5})">`)
	userIDRegexp = regexp.MustCompile(`<a href="/users/([0-9]{5})">[ \n]*<i class="fas fa-chart-bar"></i>[ \n]*My Statistics[ \n]*</a>`)
	tokenRegexp  = regexp.MustCompile(`<meta name="csrf-token" content="([A-Za-z0-9+/=]*)" />`)

	defLikeRatio    = 1.0
	defCommentRatio = 0.8

	defOrigin  = "https://www.weplusapp.com"
	defReferer = "https://www.weplusapp.com/"
	defAccept  = "text/javascript, application/javascript, application/ecmascript, application/x-ecmascript, */*; q=0.01"
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, inp *input) (string, error) {
	// Create new config.
	cfg, err := new(ctx, 15000)
	if err != nil {
		return "", err
	}

	// Get and decrypt password.
	if err := cfg.parse(inp); err != nil {
		return "", err
	}

	// Load previous states data and comments.
	data, comments, err := cfg.load(inp)
	if err != nil {
		return "", err
	}

	// Get auth token and do auth.
	if err := cfg.login(inp); err != nil {
		return "", err
	}

	// Get group ids.
	groupIds, err := cfg.getFeed(data.Group, "group", "created-at", "all", "", "0")
	if err != nil {
		return "", err
	}

	// Get company ids.
	companyIds, err := cfg.getFeed(data.Company, "company", "created-at", "image-or-video", "", "0")
	if err != nil {
		return "", err
	}

	// Create output slice.
	output := []string{}

	// Process group.
	addGroupIds, addGroupOutput, err := cfg.processGroupFeeds(groupIds, data, comments, inp)
	if err != nil {
		return "", err
	}
	data.Group = append(data.Group, addGroupIds...)
	output = append(output, addGroupOutput...)

	// Process company.
	addCompanyIds, addCompanyOutput, err := cfg.processCompanyFeeds(companyIds, data, comments, inp)
	if err != nil {
		return "", err
	}
	data.Company = append(data.Company, addCompanyIds...)
	output = append(output, addCompanyOutput...)

	// Save state data.
	if err := cfg.save(inp, data); err != nil {
		return "", err
	}

	// Create output for nothing new or mark as seen runs or truncate if it's to long.
	output = checkOutput(output, inp)

	return strings.Join(output, ""), nil
}

type cfg struct {
	ctx    context.Context
	kms    *kms.Client
	s3     *s3.Client
	client *http.Client

	userID   string
	token    string
	password string
	bucket   string
}

func new(ctx context.Context, timeout int) (*cfg, error) {
	cfg := &cfg{ctx: ctx, bucket: os.Getenv("BUCKET")}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("couldn't create a new cookie jar. %w", err)
	}

	cfg.client = &http.Client{
		Timeout: time.Duration(timeout) * time.Millisecond,
		Jar:     jar,
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't load aws default config. %w", err)
	}

	cfg.kms = kms.NewFromConfig(awsCfg)
	cfg.s3 = s3.NewFromConfig(awsCfg)

	return cfg, nil
}

type input struct {
	Email        string   `json:"email"`
	LikeRatio    *float64 `json:"likeRatio,omitempty"`
	CommentRatio *float64 `json:"commentRatio,omitempty"`
	MarkAsSeen   bool     `json:"markAsSeen"`
}

func (cfg *cfg) parse(inp *input) error {
	if inp.Email == "" {
		return fmt.Errorf("email not set in input")
	}

	if inp.LikeRatio == nil {
		inp.LikeRatio = &defLikeRatio
	}

	if inp.CommentRatio == nil {
		inp.CommentRatio = &defCommentRatio
	}

	pass, err := cfg.getPassword(inp)
	if err != nil {
		return err
	}

	cfg.password = pass
	return nil
}

func (cfg *cfg) processGroupFeeds(groupIds []string, data *data, comments [][]string, inp *input) ([]string, []string, error) {
	ids := []string{}
	output := []string{}

	for _, id := range groupIds {
		doSeen := seen(id, data.Group)
		if !doSeen && !inp.MarkAsSeen {
			if err := cfg.like(id); err != nil {
				return nil, nil, err
			}
			for _, msg := range random(comments) {
				if err := cfg.comment(id, msg); err != nil {
					return nil, nil, err
				}
			}
			row := fmt.Sprintf("liking and commenting on group post: %s for %s\n", id, inp.Email)
			output = append(output, row)
			fmt.Printf(row)
		}
		if !doSeen {
			ids = append(ids, id)
		}
	}
	return ids, output, nil
}

func (cfg *cfg) processCompanyFeeds(companyIds []string, data *data, comments [][]string, inp *input) ([]string, []string, error) {
	ids := []string{}
	output := []string{}

	for _, id := range companyIds {
		doLike, doComment, doSeen := doAction(id, data.Company, *inp.LikeRatio, *inp.CommentRatio)
		if doComment && !inp.MarkAsSeen {
			doLike = true
			for _, msg := range random(comments) {
				if err := cfg.comment(id, msg); err != nil {
					return nil, nil, err
				}
			}
			row := fmt.Sprintf("commenting on company post: %s for %s\n", id, inp.Email)
			output = append(output, row)
			fmt.Printf(row)
		}
		if doLike && !inp.MarkAsSeen {
			if err := cfg.like(id); err != nil {
				return nil, nil, err
			}
			row := fmt.Sprintf("liking company post: %s for %s\n", id, inp.Email)
			output = append(output, row)
			fmt.Printf(row)
		}
		if !doSeen {
			ids = append(ids, id)
		}
	}
	return ids, output, nil
}

func (cfg *cfg) getPassword(inp *input) (string, error) {
	str := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(inp.Email, ".", "_"), "-", "_"), "@", "_"))

	raw := os.Getenv(str)
	if raw == "" {
		return "", fmt.Errorf("couldn't read env var %s", str)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("couldn't base64 decode password for %s", str)
	}

	res, err := cfg.kms.Decrypt(cfg.ctx, &kms.DecryptInput{CiphertextBlob: decoded})
	if err != nil {
		return "", fmt.Errorf("couldn't decrypt password %s. %w", str, err)
	}

	return string(res.Plaintext), nil
}

type data struct {
	Group   []string `json:"group"`
	Company []string `json:"company"`
}

func (cfg *cfg) load(inp *input) (*data, [][]string, error) {
	email := strings.ToLower(inp.Email)
	commentsFile := fmt.Sprintf("%s.comments.txt", email)
	stateFile := fmt.Sprintf("%s.json", email)

	// Read comments data.
	rawComments, err := cfg.download(commentsFile)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't read comment data. %w", err)
	}

	comments := [][]string{}
	for _, commentPair := range strings.Split(string(rawComments), "\n") {
		if commentPair != "" {
			add := []string{}
			for _, comment := range strings.Split(commentPair, ",") {
				add = append(add, strings.TrimSpace(comment))
			}
			comments = append(comments, add)
		}
	}

	// Read personal state data.
	raw, err := cfg.download(stateFile)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") {
			if inp.MarkAsSeen {
				return &data{}, comments, nil
			}
			return nil, comments, fmt.Errorf("first load needs to be with markAsSeen true. %w", err)
		}
		return nil, comments, fmt.Errorf("couldn't read state data. %w", err)
	}

	d := &data{}
	if err := json.Unmarshal(raw, d); err != nil {
		return nil, comments, fmt.Errorf("couldn't json unmarshal body of state data. %w", err)
	}

	return d, comments, nil
}

func (cfg *cfg) download(file string) ([]byte, error) {
	resp, err := cfg.s3.GetObject(cfg.ctx, &s3.GetObjectInput{Bucket: &cfg.bucket, Key: &file})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") {
			return nil, err
		}
		return nil, fmt.Errorf("couldn't download file from s3://%s/%s. %w", cfg.bucket, file, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("couldn't read body of file from s3://%s/%s. %w", cfg.bucket, file, err)
	}

	return raw, nil
}

func (cfg *cfg) save(inp *input, data *data) error {
	email := strings.ToLower(inp.Email)

	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("couldn't json marshal state data for %s. %w", email, err)
	}

	file := fmt.Sprintf("%s.json", strings.ToLower(email))
	_, err = cfg.s3.PutObject(cfg.ctx, &s3.PutObjectInput{
		Bucket: &cfg.bucket,
		Key:    &file,
		Body:   bytes.NewReader(raw),
	})
	if err != nil {
		return fmt.Errorf("couldn't save state data to s3://%s/%s. %w", cfg.bucket, file, err)
	}

	return nil
}

type request struct {
	method      string
	url         string
	body        []byte
	contentType string
	accept      string
	origin      string
	referer     string
}

func newRequest(req *request) (*http.Request, error) {
	r, err := http.NewRequest(req.method, req.url, bytes.NewReader(req.body))
	if err != nil {
		return nil, fmt.Errorf("couldn't create new http request for %s %s. %w", req.method, req.url, err)
	}

	r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 11.2; rv:86.0) Gecko/20100101 Firefox/86.0")

	if req.contentType != "" {
		r.Header.Set("Content-Type", req.contentType)
	}

	if req.accept != "" {
		r.Header.Set("Accept", req.accept)
	}

	if req.origin != "" {
		r.Header.Set("Origin", req.origin)
	}

	if req.referer != "" {
		r.Header.Set("Referer", req.referer)
	}

	return r, nil
}

func (cfg *cfg) checkToken(body string) {
	matches := tokenRegexp.FindStringSubmatch(body)
	if len(matches) == 2 {
		cfg.token = matches[1]
	}
}

func (cfg *cfg) checkUserID(body string) {
	matches := userIDRegexp.FindStringSubmatch(body)
	if len(matches) == 2 {
		cfg.userID = matches[1]
	}
}

func (cfg *cfg) login(inp *input) error {
	if err := cfg.setAuthToken(); err != nil {
		return err
	}

	return cfg.auth(inp.Email, cfg.password)
}

func (cfg *cfg) setAuthToken() error {
	req, err := newRequest(&request{
		method: "GET",
		url:    "https://www.weplusapp.com/login",
	})
	if err != nil {
		return err
	}

	resp, err := cfg.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't send http request to %s. %w", req.URL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("couldn't read response body for %s. %w", req.URL.String(), err)
	}
	body := string(raw)

	if resp.StatusCode != 200 {
		return fmt.Errorf("expected status code 200 from get session but got %d. %s", resp.StatusCode, body)
	}

	cfg.checkToken(body)

	return nil
}

func (cfg *cfg) auth(username string, password string) error {
	payload := url.Values{}
	payload.Set("utf8", "✓")
	payload.Set("authenticity_token", cfg.token)
	payload.Set("email", username)
	payload.Set("password", password)
	payload.Set("commit", "Logga+in")

	req, err := newRequest(&request{
		method:      "POST",
		url:         "https://www.weplusapp.com/sessions",
		body:        []byte(payload.Encode()),
		contentType: "application/x-www-form-urlencoded",
		origin:      "https://www.weplusapp.com",
		referer:     "https://www.weplusapp.com/login",
	})
	if err != nil {
		return err
	}

	resp, err := cfg.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't send http request to %s. %w", req.URL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("couldn't read response body for %s. %w", req.URL.String(), err)
	}
	body := string(raw)

	if resp.StatusCode != 200 {
		return fmt.Errorf("expected status code 200 from login but got %d. %s", resp.StatusCode, body)
	}

	cfg.checkToken(body)
	cfg.checkUserID(body)

	if strings.Contains(body, "Email eller lösenord är ogiltiga") {
		return fmt.Errorf("wrong username or password")
	}

	return nil
}

func (cfg *cfg) getFeed(prev []string, feedType string, sort string, filter string, query string, offset string) ([]string, error) {
	qs := url.Values{}
	qs.Set("utf8", "✓")
	qs.Set("type", feedType)
	qs.Set("sort", sort)
	qs.Set("filter", filter)
	qs.Set("query", query)
	qs.Set("only_items", "true")
	qs.Set("offset", offset)
	req, err := newRequest(&request{
		method:  "GET",
		url:     fmt.Sprintf("https://www.weplusapp.com/feed?%s", qs.Encode()),
		referer: "https://www.weplusapp.com/",
	})
	if err != nil {
		return nil, err
	}

	resp, err := cfg.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't send http request to %s. %w", req.URL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("couldn't read response body for %s. %w", req.URL.String(), err)
	}
	body := string(raw)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("expected status code 200 from get feeds but got %d. %s", resp.StatusCode, body)
	}

	cfg.checkToken(body)

	ids := []string{}
	done := false

	idMatches := postRegexp.FindAllStringSubmatch(body, -1)
	for _, match := range idMatches {
		if len(match) != 3 {
			fmt.Printf("something went wrong when matching feed. expected length to be 3 but bot %d\n", len(match))
			continue
		}
		if match[2] == cfg.userID {
			continue
		}

		// If at least one of the ids has been seen we can stop
		// downloading new posts since we sort on created at.
		if seen(match[1], prev) {
			done = true
		}

		ids = append(ids, match[1])
	}

	// If done is true we can just return and not process anymore posts.
	if !done {
		regMore, err := regexp.Compile(fmt.Sprintf(
			`<li class="feed-more-item" data-type="%s" data-offset="([0-9]*)" data-limit="12" data-sort="%s" data-filter="%s">`,
			feedType, sort, filter,
		))
		if err != nil {
			return nil, err
		}
		moreMatches := regMore.FindStringSubmatch(body)
		if len(moreMatches) == 2 {
			new, err := cfg.getFeed(prev, feedType, sort, filter, query, moreMatches[1])
			if err != nil {
				return nil, err
			}
			ids = append(ids, new...)
		}
	}

	return ids, nil
}

func (cfg *cfg) like(id string) error {
	payload := url.Values{}
	payload.Set("like[status_id]", id)
	payload.Set("link_css_id", fmt.Sprintf("like-status-%s", id))

	req, err := newRequest(&request{
		method:      "POST",
		url:         "https://www.weplusapp.com/likes",
		body:        []byte(payload.Encode()),
		contentType: "application/x-www-form-urlencoded; charset=UTF-8",
		accept:      defAccept,
		origin:      "https://www.weplusapp.com",
		referer:     "https://www.weplusapp.com/",
	})
	if err != nil {
		return err
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CSRF-Token", cfg.token)

	resp, err := cfg.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't send http request to %s. %w", req.URL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("couldn't read response body for %s. %w", req.URL.String(), err)
	}
	body := string(raw)

	if resp.StatusCode != 200 {
		return fmt.Errorf("expected status code 200 from like but got %d. %s", resp.StatusCode, body)
	}

	cfg.checkToken(body)

	return nil
}

func (cfg *cfg) comment(id string, comment string) error {
	payload := url.Values{}
	payload.Set("comment[body]", comment)
	payload.Set("comment[status_id]", id)
	payload.Set("comments_css_id", fmt.Sprintf("comments-list-%s", id))

	req, err := newRequest(&request{
		method:      "POST",
		url:         "https://www.weplusapp.com/comments",
		body:        []byte(payload.Encode()),
		contentType: "application/x-www-form-urlencoded; charset=UTF-8",
		accept:      defAccept,
		origin:      defOrigin,
		referer:     defReferer,
	})
	if err != nil {
		return err
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CSRF-Token", cfg.token)

	resp, err := cfg.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't send http request to %s. %w", req.URL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("couldn't read response body for %s. %w", req.URL.String(), err)
	}
	body := string(raw)

	if resp.StatusCode != 200 {
		return fmt.Errorf("expected status code 200 from comment but got %d. %s", resp.StatusCode, body)
	}

	cfg.checkToken(body)

	return nil
}

func seen(id string, slice []string) bool {
	for _, ssid := range slice {
		if id == ssid {
			return true
		}
	}
	return false
}

func doAction(id string, slice []string, likeRatio float64, commentRatio float64) (bool, bool, bool) {
	doSeen := seen(id, slice)
	if doSeen {
		return false, false, doSeen
	}

	like := rand.Float64() < likeRatio
	comment := rand.Float64() < commentRatio

	return like, comment, doSeen
}

func random(comments [][]string) []string {
	rand.Seed(time.Now().UnixNano())
	return comments[rand.Intn(len(comments))]
}

func checkOutput(output []string, inp *input) []string {
	if len(output) == 0 {
		switch inp.MarkAsSeen {
		case true:
			output = append(output, "state saved up until now! you can now run in it normally")
		case false:
			output = append(output, "nothing liked or commented since last run!")
		}
	}

	if len(output) > 200 {
		output = output[0:199]
		output = append(output, "output was truncated due to it being to long ...")
	}

	return output
}
