/*
Copyright 2017 Vector Creations Ltd

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/go-github/github"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var maxPayloadSize = 1024 * 1024 * 55 // 55 MB

type submitServer struct {
	// github client for reporting bugs. may be nil, in which case,
	// reporting is disabled.
	ghClient *github.Client

	// External URI to /api
	apiPrefix string

	// mappings from application to github owner/project
	githubProjectMappings map[string]string
}

type payload struct {
	Text      string            `json:"text"`
	AppName   string            `json:"app"`
	Version   string            `json:"version"`
	UserAgent string            `json:"user_agent"`
	Logs      []logEntry        `json:"logs"`
	Data      map[string]string `json:"data"`
}

type logEntry struct {
	ID    string `json:"id"`
	Lines string `json:"lines"`
}

func (s *submitServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" && req.Method != "OPTIONS" {
		respond(405, w)
		return
	}

	// Set CORS
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	if req.Method == "OPTIONS" {
		respond(200, w)
		return
	}

	p := parseRequest(w, req)
	if p == nil {
		// parseRequest already wrote an error
		return
	}

	if err := s.saveReport(req.Context(), *p); err != nil {
		log.Println("Error handling report", err)
		http.Error(w, "Internal error", 500)
		return
	}

	respond(200, w)
}

// parseRequest attempts to parse a received request as a bug report. If
// the request cannot be parsed, it responds with an error and returns nil.
func parseRequest(w http.ResponseWriter, req *http.Request) *payload {
	length, err := strconv.Atoi(req.Header.Get("Content-Length"))
	if err != nil {
		log.Println("Couldn't parse content-length", err)
		http.Error(w, "Bad content-length", 400)
		return nil
	}
	if length > maxPayloadSize {
		log.Println("Content-length", length, "too large")
		http.Error(w, fmt.Sprintf("Content too large (max %i)", maxPayloadSize), 413)
		return nil
	}
	var p payload
	if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
		log.Println("Couldn't decode request body", err)
		http.Error(w, fmt.Sprintf("Could not decode payload: %s", err.Error()), 400)
		return nil
	}

	p.Text = strings.TrimSpace(p.Text)

	if p.Data == nil {
		p.Data = make(map[string]string)
	}

	// backwards-compatibility hack: current versions of riot-android
	// don't set 'app', so we don't correctly file github issues.
	if p.AppName == "" && p.UserAgent == "Android" {
		p.AppName = "riot-android"

		// they also shove lots of stuff into 'Version' which we don't really
		// want in the github report
		for _, line := range strings.Split(p.Version, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			key := strings.TrimSpace(parts[0])
			val := ""
			if len(parts) > 1 {
				val = strings.TrimSpace(parts[1])
			}
			p.Data[key] = val
		}
		p.Version = ""
	}

	return &p
}

func (s *submitServer) saveReport(ctx context.Context, p payload) error {
	// Dump bug report to disk as form:
	//  "bugreport-20170115-112233.log.gz" => user text, version, user agent, # logs
	//  "bugreport-20170115-112233-0.log.gz" => most recent log
	//  "bugreport-20170115-112233-1.log.gz" => ...
	//  "bugreport-20170115-112233-N.log.gz" => oldest log
	t := time.Now().UTC()
	prefix := t.Format("2006-01-02/150405")
	listingURL := s.apiPrefix + "/listing/" + prefix

	log.Println("Handling report submission; listing URI will be", listingURL)

	var summaryBuf bytes.Buffer
	fmt.Fprintf(
		&summaryBuf,
		"%s\n\nNumber of logs: %d\nApplication: %s\nVersion: %s\nUser-Agent: %s\n",
		p.Text, len(p.Logs), p.AppName, p.Version, p.UserAgent,
	)
	for k, v := range p.Data {
		fmt.Fprintf(&summaryBuf, "%s: %s\n", k, v)
	}
	if err := gzipAndSave(summaryBuf.Bytes(), prefix, "details.log.gz"); err != nil {
		return err
	}

	for i, log := range p.Logs {
		if err := gzipAndSave([]byte(log.Lines), prefix, fmt.Sprintf("logs-%d.log.gz", i)); err != nil {
			return err // TODO: Rollback?
		}
	}

	if s.ghClient == nil {
		// we're done here
		log.Println("GH issue submission disabled")
		return nil
	}

	// submit a github issue
	ghProj := s.githubProjectMappings[p.AppName]
	if ghProj == "" {
		log.Println("Not creating GH issue for unknown app", p.AppName)
		return nil
	}
	splits := strings.SplitN(ghProj, "/", 2)
	if len(splits) < 2 {
		log.Println("Can't create GH issue for invalid repo", ghProj)
	}
	owner, repo := splits[0], splits[1]

	issueReq := buildGithubIssueRequest(p, listingURL)

	issue, _, err := s.ghClient.Issues.Create(ctx, owner, repo, &issueReq)
	if err != nil {
		return err
	}

	log.Println("Created issue:", *issue.HTMLURL)

	return nil
}

func buildGithubIssueRequest(p payload, listingURL string) github.IssueRequest {
	var title string
	if p.Text == "" {
		title = "Untitled report"
	} else {
		// set the title to the first line of the user's report
		if i := strings.IndexAny(p.Text, "\r\n"); i < 0 {
			title = p.Text
		} else {
			title = p.Text[0:i]
		}
	}

	body := fmt.Sprintf(
		"User message:\n```\n%s\n```\nVersion: %s\n[Details](%s) / [Logs](%s)",
		p.Text,
		p.Version,
		listingURL+"/details.log.gz",
		listingURL,
	)

	return github.IssueRequest{
		Title: &title,
		Body:  &body,
	}
}

func respond(code int, w http.ResponseWriter) {
	w.WriteHeader(code)
	w.Write([]byte("{}"))
}

func gzipAndSave(data []byte, dirname, fpath string) error {
	_ = os.MkdirAll(filepath.Join("bugs", dirname), os.ModePerm)
	fpath = filepath.Join("bugs", dirname, fpath)

	if _, err := os.Stat(fpath); err == nil {
		return fmt.Errorf("file already exists") // the user can just retry
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		return err
	}
	if err := gz.Flush(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := ioutil.WriteFile(fpath, b.Bytes(), 0644); err != nil {
		return err
	}
	return nil
}