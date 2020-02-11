package core

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
	"github.com/spf13/cobra"
	"github.com/theblackturtle/gospider/stringset"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Crawler struct {
	cmd      *cobra.Command
	C        *colly.Collector
	Output   *Output
	domainRe *regexp.Regexp

	subSet  *stringset.StringFilter
	awsSet  *stringset.StringFilter
	jsSet   *stringset.StringFilter
	urlSet  *stringset.StringFilter
	formSet *stringset.StringFilter

	site   *url.URL
	domain string
}

func NewCrawler(site *url.URL, cmd *cobra.Command) *Crawler {
	domain := GetDomain(site)
	if domain == "" {
		Logger.Error("Failed to parse domain")
		os.Exit(1)
	}
	Logger.Infof("Crawling site: %s", site)

	maxDepth, _ := cmd.Flags().GetInt("depth")
	concurrent, _ := cmd.Flags().GetInt("concurrent")
	delay, _ := cmd.Flags().GetInt("delay")
	randomDelay, _ := cmd.Flags().GetInt("random-delay")

	c := colly.NewCollector(
		colly.Async(true),
		colly.MaxDepth(maxDepth),
		colly.IgnoreRobotsTxt(),
	)

	// Setup http client
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   60 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	timeout, _ := cmd.Flags().GetInt("timeout")
	if timeout > 0 {
		c.SetRequestTimeout(time.Duration(timeout) * time.Second)
	}

	// Set proxy
	proxy, _ := cmd.Flags().GetString("proxy")
	if proxy != "" {
		Logger.Debugf("Proxy: %s", proxy)
		pU, err := url.Parse(proxy)
		if err != nil {
			Logger.Error("Failed to set proxy")
		} else {
			tr.Proxy = http.ProxyURL(pU)
		}
	}
	c.WithTransport(tr)

	// Get headers here to overwrite if "burp" flag used
	burpFile, _ := cmd.Flags().GetString("burp")
	if burpFile != "" {
		bF, err := os.Open(burpFile)
		if err != nil {
			Logger.Errorf("Failed to open Burp File: %s", err)
		} else {
			rd := bufio.NewReader(bF)
			req, err := http.ReadRequest(rd)
			if err != nil {
				Logger.Errorf("Failed to Parse Raw Request in %s: %s", burpFile, err)
			} else {
				// Set cookie
				c.OnRequest(func(r *colly.Request) {
					r.Headers.Set("Cookie", GetRawCookie(req.Cookies()))
				})

				// Set headers
				c.OnRequest(func(r *colly.Request) {
					for k, v := range req.Header {
						r.Headers.Set(strings.TrimSpace(k), strings.TrimSpace(v[0]))
					}
				})

			}
		}
	}

	// Set cookies
	cookie, _ := cmd.Flags().GetString("cookie")
	if cookie != "" && burpFile == "" {
		c.OnRequest(func(r *colly.Request) {
			r.Headers.Set("Cookie", cookie)
		})
	}

	// Set headers
	headers, _ := cmd.Flags().GetStringArray("header")
	if burpFile == "" {
		for _, h := range headers {
			headerArgs := strings.SplitN(h, ":", 2)
			headerKey := strings.TrimSpace(headerArgs[0])
			headerValue := strings.TrimSpace(headerArgs[1])
			c.OnRequest(func(r *colly.Request) {
				r.Headers.Set(headerKey, headerValue)
			})
		}
	}

	// Set User-Agent
	randomUA, _ := cmd.Flags().GetString("user-agent")
	switch ua := strings.ToLower(randomUA); {
	case ua == "mobi":
		extensions.RandomMobileUserAgent(c)
	case ua == "web":
		extensions.RandomUserAgent(c)
	default:
		c.UserAgent = ua
	}

	// Set referer
	extensions.Referer(c)

	// Disable redirect
	noRedirect, _ := cmd.Flags().GetBool("no-redirect")
	if noRedirect {
		c.SetRedirectHandler(func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		})
	}

	// Init Output
	var output *Output
	outputFolder, _ := cmd.Flags().GetString("output")
	if outputFolder != "" {
		filename := strings.ReplaceAll(site.Hostname(), ".", "_")
		output = NewOutput(outputFolder, filename)
	}

	// Set url whitelist regex
	domainRe := regexp.MustCompile(domain)
	c.URLFilters = append(c.URLFilters, domainRe)

	// Set Limit Rule
	err := c.Limit(&colly.LimitRule{
		DomainGlob:  domain,
		Parallelism: concurrent,
		Delay:       time.Duration(delay) * time.Second,
		RandomDelay: time.Duration(randomDelay) * time.Second,
	})
	if err != nil {
		Logger.Errorf("Failed to set Limit Rule: %s", err)
		os.Exit(1)
	}

	// Set blacklist url regex
	disallowedRegex := `.(jpg|jpeg|gif|css|tif|tiff|png|ttf|woff|woff2|ico)(?:\?|#|$)`
	c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(disallowedRegex))

	// Set optional blacklist url regex
	blacklists, _ := cmd.Flags().GetString("blacklist")
	if blacklists != "" {
		c.DisallowedURLFilters = append(c.DisallowedURLFilters, regexp.MustCompile(blacklists))
	}

	return &Crawler{
		cmd:      cmd,
		C:        c,
		site:     site,
		domain:   domain,
		Output:   output,
		domainRe: domainRe,
		urlSet:   stringset.NewStringFilter(),
		subSet:   stringset.NewStringFilter(),
		jsSet:    stringset.NewStringFilter(),
		formSet:  stringset.NewStringFilter(),
		awsSet:   stringset.NewStringFilter(),
	}
}

func (crawler *Crawler) Start() {
	// Handle url
	crawler.C.OnHTML("[href]", func(e *colly.HTMLElement) {
		urlString := e.Request.AbsoluteURL(e.Attr("href"))
		urlString = FixUrl(urlString, crawler.site)
		if urlString == "" {
			return
		}
		if !crawler.urlSet.Duplicate(urlString) {
			_ = e.Request.Visit(urlString)
		}
	})

	// Handle form
	crawler.C.OnHTML("form[action]", func(e *colly.HTMLElement) {
		formUrl := e.Request.AbsoluteURL(e.Attr("action"))
		formUrl = FixUrl(formUrl, crawler.site)
		if formUrl == "" {
			return
		}
		// Just print
		if !crawler.formSet.Duplicate(formUrl) {
			if crawler.domainRe.MatchString(formUrl) {
				outputFormat := fmt.Sprintf("[form] - %s", formUrl)
				fmt.Println(outputFormat)
				if crawler.Output != nil {
					crawler.Output.WriteToFile(outputFormat)
				}
			}
		}
	})

	// Find Upload Form
	uploadFormSet := stringset.NewStringFilter()
	crawler.C.OnHTML(`input[type="file"]`, func(e *colly.HTMLElement) {
		uploadUrl := e.Request.URL.String()
		if !uploadFormSet.Duplicate(uploadUrl) {
			outputFormat := fmt.Sprintf("[upload-form] - %s", uploadUrl)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}

	})

	// Handle js files
	crawler.C.OnHTML("[src]", func(e *colly.HTMLElement) {
		jsFileUrl := e.Request.AbsoluteURL(e.Attr("src"))
		jsFileUrl = FixUrl(jsFileUrl, crawler.site)
		if jsFileUrl == "" {
			return
		}

		fileExt := GetExtType(jsFileUrl)
		if fileExt == ".js" || fileExt == ".xml" || fileExt == ".json" {
			if !crawler.jsSet.Duplicate(jsFileUrl) {
				outputFormat := fmt.Sprintf("[javascript] - %s", jsFileUrl)
				fmt.Println(outputFormat)
				if crawler.Output != nil {
					crawler.Output.WriteToFile(outputFormat)
				}

				// If JS file is minimal format. Try to find original format
				if strings.Contains(jsFileUrl, ".min.js") {
					originalJS := strings.ReplaceAll(jsFileUrl, ".min.js", ".js")
					crawler.linkFinder(originalJS)
				}

				// Request and Get JS link
				crawler.linkFinder(jsFileUrl)
			}
		}
	})

	crawler.C.OnResponse(func(response *colly.Response) {
		respStr := DecodeChars(string(response.Body))

		crawler.findSubdomains(respStr)
		crawler.findAWSS3(respStr)

		// Verify which links are working
		u := response.Request.URL.String()
		outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)
		fmt.Println(outputFormat)
		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
	})

	crawler.C.OnError(func(response *colly.Response, err error) {
		Logger.Debugf("Error request: %s - Status code: %v - Error: %s", response.Request.URL.String(), response.StatusCode, err)
		// Status == 0 mean "The server IP address could not be found."
		if response.StatusCode == 404 || response.StatusCode == 429 || response.StatusCode == 0 {
			return
		}

		// Retry if status code == 999
		if response.StatusCode == 999 {
			_ = response.Request.Retry()
			return
		}

		u := response.Request.URL.String()
		outputFormat := fmt.Sprintf("[url] - [code-%d] - %s", response.StatusCode, u)
		fmt.Println(outputFormat)
		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
	})

	_ = crawler.C.Visit(crawler.site.String())
}

// Find subdomains from response
func (crawler *Crawler) findSubdomains(resp string) {
	subs := GetSubdomains(resp, crawler.domain)
	for _, sub := range subs {
		if !crawler.subSet.Duplicate(sub) {
			outputFormat := fmt.Sprintf("[subdomains] - %s", sub)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// Find AWS S3 from response
func (crawler *Crawler) findAWSS3(resp string) {
	aws := GetAWSS3(resp)
	for _, e := range aws {
		if !crawler.awsSet.Duplicate(e) {
			outputFormat := fmt.Sprintf("[aws-s3] - %s", e)
			fmt.Println(outputFormat)
			if crawler.Output != nil {
				crawler.Output.WriteToFile(outputFormat)
			}
		}
	}
}

// This function will request and parse external javascript
// and pass to main collector with scope setup
func (crawler *Crawler) linkFinder(jsUrl string) {
	client := http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(jsUrl)
	if err != nil || resp.StatusCode != 200 {
		return
	}
	// if the js file exists
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	respStr := string(body)

	crawler.findAWSS3(respStr)
	crawler.findSubdomains(respStr)

	links, err := LinkFinder(respStr)
	if err != nil {
		Logger.Error(err)
		return
	}

	for _, link := range links {
		// If link is url, check with regex to make sure it in scope
		_, err := url.Parse(link)
		if err == nil {
			if !crawler.domainRe.MatchString(link) {
				continue
			}
		}

		if strings.HasPrefix(link, "//") {
			newLink := "https:" + link
			if !crawler.domainRe.MatchString(newLink) {
				continue
			}
			link = newLink
		}

		// JS Regex Result
		outputFormat := fmt.Sprintf("[linkfinder] - [from: %s] - %s", jsUrl, link)
		fmt.Println(outputFormat)
		if crawler.Output != nil {
			crawler.Output.WriteToFile(outputFormat)
		}
		// Try to request JS path
		_ = crawler.C.Visit(FixUrl(link, crawler.site))
	}
}
