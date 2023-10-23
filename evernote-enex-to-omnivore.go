package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PurpleSec/escape"
	"github.com/integrii/flaggy"
	uuid "github.com/satori/go.uuid"
	"github.com/wormi4ok/evernote2md/encoding/enex"
)

var regexpsCompiled []*regexp.Regexp
var previewMode bool
var enexInputFiles string
var resumeFrom string
var skipIds string
var omnivoreUrl string = "https://api-prod.omnivore.app/api/graphql"
var omnivoreAPIKey string = "547e1bcd-c948-4ce1-9a1f-0a8831be4840"
var alreadyPublishedIDFilename = ".cache"
var processCount int = -1

var results map[string]*NoteProcessingResult

type NoteProcessingResult struct {
	uuid                     string
	url                      string
	processed                bool
	stillOnline              bool
	processedAsArticle       bool
	processedAsURL           bool
	savedAsArticleSuccessful bool
	savedAsURLSuccessful     bool
}

func buildDeterministicGUID(content string) string {
	// calculate the MD5 hash of the
	// combination of organisation
	// and account reference
	md5hash := md5.New()
	md5hash.Write([]byte(content))

	// convert the hash value to a string
	md5string := hex.EncodeToString(md5hash.Sum(nil))

	// generate the UUID from the
	// first 16 bytes of the MD5 hash
	uuid, err := uuid.FromBytes([]byte(md5string[0:16]))
	if err != nil {
		log.Fatal(err)
	}

	return uuid.String()
}

func initRegexp() {
	regexpsAsString := []string{"[\\?;]utm_source.*$",
		"[\\?;&]utm_campaign.*$",
		"[\\?;&]mkt_tok.*$",
		"[\\?;&]utm_medium.*$",
		"[\\?;&]utm_term.*$",
		"[\\?;&]ul_campaign.*$",
		"[\\?;&]ul_source.*$",
		"\\?%24deep_link.*$",
		"\\?idg_eid.*$",
		"\\?source=.*$",
		"\\?$"}

	regexpsCompiled = []*regexp.Regexp{}
	for _, regexpAsString := range regexpsAsString {
		m := regexp.MustCompile(regexpAsString)
		regexpsCompiled = append(regexpsCompiled, m)
	}
}

func cleanUrl(s string) string {
	// 's/[\?;]utm_source.*$//' -e 's/[\?;]utm_campaign.*$//' -e 's/\?mkt_tok.*$//' -e 's/\?utm_medium.*$//' -e 's/\?utm_term.*$//' -e 's/\?%24deep_link.*$//' -e 's/\?idg_eid.*$//'

	for _, matcher := range regexpsCompiled {
		s = matcher.ReplaceAllString(s, "")
	}

	return s

}

func checkOnlineStatus(url string) (bool, int) {

	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, 0
	}

	req.Header.Add("Accept", `text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8`)
	req.Header.Add("User-Agent", `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_7_5) AppleWebKit/537.11 (KHTML, like Gecko) Chrome/23.0.1271.64 Safari/537.11`)

	response, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case 401:
		return false, response.StatusCode
	case 403:
		return false, response.StatusCode
	case 404:
		return false, response.StatusCode
	case 500:
		return false, response.StatusCode
	case 503:
		return false, response.StatusCode
	default:
		return true, 0
	}
}

func process(previewMode bool, enexInputFilesToProcess []string, resumeFrom string, idsToBeSkipped []string) error {

	awaitForSpecificID := false
	specificIdFound := false
	if len(resumeFrom) != 0 {
		awaitForSpecificID = true
	}

	cnt := 0

	for _, s := range enexInputFilesToProcess {

		fmt.Println("Starting to process file [" + s + "]")

		fd, err := os.Open(s)
		if err != nil {
			return err
		}

		d, err := enex.NewStreamDecoder(fd)
		if err != nil {
			return err
		}

		for {
			note := enex.Note{}
			if err := d.Next(&note); err != nil {
				if err != io.EOF {
					log.Printf("Failed to decode the next note: %s", err)
				}
				break
			}
			sourceUrl := cleanUrl(strings.TrimSpace(note.Attributes.SourceUrl))
			uuid := buildDeterministicGUID(sourceUrl)
			if len(sourceUrl) == 0 {
				uuid = buildDeterministicGUID(note.Title)
				sourceUrl = "https://evernote.com"
			}

			skip := false
			skipReason := ""
			for _, token := range idsToBeSkipped {
				if token == uuid {
					skip = true
					skipReason = "skipped ID from CLI"
				}
			}

			if isInCache(uuid) {
				skip = true
				skipReason = "skipped ID from .cache previous file"
			}

			if (!awaitForSpecificID || (awaitForSpecificID && specificIdFound)) && !skip {
				results[uuid] = &NoteProcessingResult{uuid: uuid, url: sourceUrl, processed: true}
				labels := []string{"IMPORT/Evernote"}
				formattedLabels := buildLabels(labels)
				fomattedCreatedAt := buildFormattedDate(note.Created)
				fmt.Println("")
				fmt.Println(uuid + " | " + strings.Join(labels, ", ") + " | " + sourceUrl)

				urlAccessible, errorCode := checkOnlineStatus(sourceUrl)

				processAsURL := false
				processAsArticle := false

				if len(sourceUrl) == 0 {
					fmt.Println("  > [WARNING] No source URL, will save as article")
					processAsArticle = true
				} else {
					if !urlAccessible {
						fmt.Println("  > [WARNING] url [" + sourceUrl + "] is not accessible anymore (error " + strconv.Itoa(errorCode) + "), will save as article")
						results[uuid].stillOnline = false
						processAsArticle = true
					} else {
						fmt.Println("  > [INFO] url [" + sourceUrl + "] still accessible, will save as URL")
						results[uuid].stillOnline = true
						processAsURL = true
					}
				}

				if !previewMode {
					if processAsArticle {
						publishAsArticle(sourceUrl, uuid, formattedLabels, "api", fomattedCreatedAt, "ARCHIVED", string(note.Content), note.Title)
					}

					if processAsURL {
						publishAsURL(sourceUrl, uuid, formattedLabels, "api", fomattedCreatedAt, "ARCHIVED")
					}

					if results[uuid].savedAsURLSuccessful || results[uuid].savedAsArticleSuccessful {
						cnt++
						putInCache(uuid)
					}

					if processCount != -1 && cnt == processCount {
						fmt.Println("Stopping, as [" + strconv.Itoa(cnt) + "] entries have been processed")
						break
					}
				}

			} else {
				if results[uuid] == nil {
					results[uuid] = &NoteProcessingResult{uuid: uuid, url: sourceUrl, processed: false} // in case of duplicates, some entries will be overriden here in results ?
				}
				fmt.Println("")
				fmt.Println(uuid + " | SKIPPED " + "(" + skipReason + ") | " + sourceUrl)
			}

			if uuid == resumeFrom {
				specificIdFound = true
			}
		}
	}

	return nil
}

func putInCache(uuid string) {
	f, _ := os.OpenFile(alreadyPublishedIDFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer f.Close()
	f.WriteString(uuid + "\n")
}

func isInCache(uuid string) bool {
	uuid = strings.TrimSpace(uuid)
	content, _ := os.ReadFile(alreadyPublishedIDFilename)
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == uuid {
			return true
		}
	}
	return false
}

func buildFormattedDate(created string) string {
	// 20231021T100502Z
	inputLayout := "20060102T150405Z"
	t, err := time.Parse(inputLayout, created)
	if err != nil {
		fmt.Println(err)
		return ""
	}

	outputLayout := "2006-01-02"
	return t.Format(outputLayout)
}

func buildLabels(labels []string) string {
	s := ""
	sep := ""
	for _, label := range labels {
		s = s + sep + "{ \"name\": \"" + label + "\" }"
		sep = ","
	}
	return "[ " + s + " ]"
}

func publishAsArticle(sourceUrl string, uuid string, labels string, api string, savedAt string, status string, content string, title string) {
	results[uuid].processedAsArticle = true
	results[uuid].savedAsArticleSuccessful = true

	var json = []byte(`{ "query": 
		"mutation SavePage($input: SavePageInput!) { savePage(input: $input) { ... on SaveSuccess { url clientRequestId } ... on SaveError { errorCodes message } } }", 
		"variables": 
			{ 
				"input": { 
					"savedAt": "` + savedAt + `", 
					"labels": ` + labels + `, 
					"clientRequestId": "` + uuid + `", 
					"source": "` + api + `", 
					"originalContent": ` + escape.JSON(content) + `, 
					"title": ` + escape.JSON(title) + `, 
					"url": "` + sourceUrl + `", 
					"state": "` + status + `" }} }`)
	err := publish(json)
	if err != nil {
		fmt.Println(err)
		results[uuid].savedAsArticleSuccessful = false
	}

}

func publishAsURL(sourceUrl string, uuid string, labels string, api string, savedAt string, status string) {
	results[uuid].processedAsURL = true
	results[uuid].savedAsURLSuccessful = true

	var json = []byte(`{ "query": 
		"mutation SaveUrl($input: SaveUrlInput!) { saveUrl(input: $input) { ... on SaveSuccess { url clientRequestId } ... on SaveError { errorCodes message } } }", 
		"variables": 
			{ 
				"input": { 
					"savedAt": "` + savedAt + `", 
					"labels": ` + labels + `, 
					"clientRequestId": "` + uuid + `", 
					"source": "` + api + `", 
					"url": "` + sourceUrl + `", 
					"state": "` + status + `" }} }`)
	err := publish(json)
	if err != nil {
		fmt.Println(err)
		results[uuid].savedAsURLSuccessful = false
	}
}

func publish(json []byte) error {
	req, err := http.NewRequest("POST", omnivoreUrl, bytes.NewBuffer(json))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authorization", omnivoreAPIKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		fmt.Println("  > OMNIVORE request" + string(json))
		fmt.Println("  > [ERROR] OMNIVORE Response Status:", resp.Status)
		fmt.Println("  > [ERROR] OMNIVORE Response Headers:", resp.Header)
		fmt.Println("  > [ERROR] OMNIVORE Response Body:", string(body))
		return fmt.Errorf("wrong error code")
	} else {
		fmt.Println("  > [INFO] Correctly saved to Omnivore : ", string(body))
		return nil
	}
}

func extractTokensFromCLIParameter(s string) []string {
	results := []string{}
	for _, f := range strings.Split(s, ",") {
		results = append(results, strings.TrimSpace(f))
	}
	return results
}

func displayResults() {
	nbOfProcessedItems := 0
	nbOfProcessedItemsAsArticles := 0
	nbOfProcessedItemsAsURLs := 0
	nbOfErroenousItemsSavedAsArticles := 0
	nbOfErroenousItemsSavedAsURLs := 0
	for _, result := range results {
		if result.processed {
			nbOfProcessedItems++

			if result.processedAsURL {
				nbOfProcessedItemsAsURLs++
				if !result.savedAsURLSuccessful {
					nbOfErroenousItemsSavedAsURLs++
				}
			}

			if result.processedAsArticle {
				nbOfProcessedItemsAsArticles++
				if !result.savedAsArticleSuccessful {
					nbOfErroenousItemsSavedAsArticles++
				}
			}
		}
	}

	fmt.Println("")
	fmt.Println("===================================================")
	fmt.Println("Total number of items processed : " + strconv.Itoa(nbOfProcessedItems))
	fmt.Println("Total number of items processed as URL : " + strconv.Itoa(nbOfProcessedItemsAsURLs))
	fmt.Println("Total number of items processed as Article : " + strconv.Itoa(nbOfProcessedItemsAsArticles))
	fmt.Println("Total number of errors while saving as URL : " + strconv.Itoa(nbOfErroenousItemsSavedAsURLs))
	fmt.Println("Total number of errors while saving as Article : " + strconv.Itoa(nbOfErroenousItemsSavedAsArticles))
}

func main() {

	results = make(map[string]*NoteProcessingResult)

	// Input line parameters
	flaggy.String(&omnivoreAPIKey, "a", "api", "OMNIVORE APIKey")
	flaggy.String(&omnivoreUrl, "u", "url", "OMNIVORE Graphql HTTP endpoint / URL (optional)")
	flaggy.String(&enexInputFiles, "i", "input", "Input files, comma separated (like '-i file1.enex,file2.enex')")
	flaggy.Bool(&previewMode, "p", "preview", "Activate preview mode (optional)")
	flaggy.String(&resumeFrom, "r", "resume-from", "ID (hash) of the last valid URL : only the following URL will be processed (optional)")
	flaggy.String(&skipIds, "s", "skip", "IDs (hash) to be skipped (like '-s ID1,ID2') (optional)")
	flaggy.Int(&processCount, "c", "count", "Number of items to process (optional, default -1)")

	flaggy.Parse()

	initRegexp()

	if processCount == -1 {
		fmt.Println("All items will be processed (except skipped ones)")
	} else {
		fmt.Println("Only [" + strconv.Itoa(processCount) + "] items will be processed before stopping")
	}

	if previewMode {
		fmt.Println("Preview mode activated : nothing will be sent to OMNIVORE, but full parsing of input file will be done")
	}

	if len(enexInputFiles) == 0 {
		fmt.Println("Please provide input files with --input")
		os.Exit(3)
	}

	enexInputFilesToProcess := extractTokensFromCLIParameter(enexInputFiles)
	fmt.Println("Files to be processed : " + strings.Join(enexInputFilesToProcess, ", "))
	fmt.Println("OMNIVORE URL [" + omnivoreUrl + "], OMNIVORE APIKey [" + omnivoreAPIKey + "]")

	if len(resumeFrom) != 0 {
		resumeFrom = strings.TrimSpace(resumeFrom)
		fmt.Println("Only URL *after* this ID will be processed [" + resumeFrom + "]")
	}

	idsToBeSkipped := extractTokensFromCLIParameter(skipIds)
	if len(idsToBeSkipped) > 0 {
		fmt.Println("URLs IDs to be skipped : " + strings.Join(idsToBeSkipped, ", "))
	}

	if err := process(previewMode, enexInputFilesToProcess, resumeFrom, idsToBeSkipped); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	displayResults()
}
