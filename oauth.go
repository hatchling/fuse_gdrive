package main

// oauth.go contains utility functions for managing oauth connections.

// Credit to the Go examples, from which this is mostly copied:
// https://code.google.com/p/google-api-go-client/source/browse/examples
// and which are Copyright (c) 2011 Google Inc. All rights reserved.

import (
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"code.google.com/p/goauth2/oauth"
)

const (
	defaultClientId string = "902751591868-ghc6jn2vquj6s8n5v5np2i66h3dh5pqq.apps.googleusercontent.com"
	defaultSecret   string = "LLsUuv2NoLglNKx14t5dA9SC"
)

var (
	clientId   = flag.String("clientid", "", "OAuth Client ID")
	secret     = flag.String("secret", "", "OAuth Client Secret")
	cacheToken = flag.Bool("cachetoken", true, "cache the OAuth token")
	httpDebug  = flag.Bool("http.debug", false, "show HTTP traffic")
)

func tokenCacheFile(config *oauth.Config) string {
	hash := fnv.New32a()
	hash.Write([]byte(config.ClientId))
	hash.Write([]byte(config.ClientSecret))
	hash.Write([]byte(config.Scope))
	fn := fmt.Sprintf("fuse-gdrive-token-%v", hash.Sum32())
	return filepath.Join(osDataDir(), url.QueryEscape(fn))
}

func tokenFromFile(file string) (*oauth.Token, error) {
	if !*cacheToken {
		return nil, errors.New("--cachetoken is false")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	t := new(oauth.Token)
	err = gob.NewDecoder(f).Decode(t)
	return t, err
}

func saveToken(file string, token *oauth.Token) {
	dataDir := osDataDir()
	_, err := os.Stat(dataDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dataDir, 0700); err != nil {
			log.Printf("Warning: failed to cache oauth token: cache dir does not exist, could not create it: %v", err)
			return
		}
	}
	f, err := os.Create(file)
	if err != nil {
		log.Printf("Warning: failed to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	gob.NewEncoder(f).Encode(token)
}

func tokenFromWeb(config *oauth.Config) *oauth.Token {
	ch := make(chan string)
	randState := fmt.Sprintf("st%d", time.Now().UnixNano())
	http.HandleFunc("/auth", func(rw http.ResponseWriter, req *http.Request) {
		if req.FormValue("state") != randState {
			log.Printf("State doesn't match: req = %#v", req)
			http.Error(rw, "", 500)
			return
		}
		if code := req.FormValue("code"); code != "" {
			fmt.Fprintf(rw, "<h1>Success</h1>Authorized.")
			rw.(http.Flusher).Flush()
			ch <- code
			return
		}
		log.Printf("no code")
		http.Error(rw, "", 500)
	})

	config.RedirectURL = fmt.Sprintf("http://localhost:%s/auth", *port)
	authUrl := config.AuthCodeURL(randState)
	go openUrl(authUrl)
	log.Printf("Authorize this app at: %s", authUrl)
	code := <-ch
	log.Printf("Got code: %s", code)

	t := &oauth.Transport{
		Config:    config,
		Transport: http.DefaultTransport,
	}
	_, err := t.Exchange(code)
	if err != nil {
		log.Fatalf("Token exchange error: %v", err)
	}
	return t.Token
}

func openUrl(url string) {
	try := []string{"xdg-open", "google-chrome", "open"}
	for _, bin := range try {
		err := exec.Command(bin, url).Run()
		if err == nil {
			return
		}
	}
	log.Printf("Error opening URL in browser.")
}

func getOAuthClient(scope string) *http.Client {
	// TODO: offer to cache clientid & secret if provided by flag
	c := defaultClientId
	s := defaultSecret
	if *clientId != "" && *secret != "" {
		c = *clientId
		s = *secret
	}

	var config = &oauth.Config{
		ClientId:     c,
		ClientSecret: s,
		Scope:        scope, // of access requested (drive, gmail, etc)
		AuthURL:      "https://accounts.google.com/o/oauth2/auth",
		TokenURL:     "https://accounts.google.com/o/oauth2/token",
	}

	cacheFile := tokenCacheFile(config)
	token, err := tokenFromFile(cacheFile)
	if err != nil {
		token = tokenFromWeb(config)
		saveToken(cacheFile, token)
	} else {
		log.Printf("Using cached token %#v from %q", token, cacheFile)
	}

	t := &oauth.Transport{
		Token:     token,
		Config:    config,
		Transport: http.DefaultTransport,
	}
	return t.Client()
}

// This is a terrible but simple hack.  The better fix is coming.
func tokenKicker(client *http.Client, interval time.Duration) {
	transport, ok := client.Transport.(*oauth.Transport)
	if !ok {
		log.Println("tokenKicker client must be an oauth client!")
		return
	}
	log.Printf("access token expires: %s\n", transport.Token.Expiry)
	for {
		time.Sleep(interval)
		if err := transport.Refresh(); err != nil {
			log.Println("access token refresh failure: ", err)
		} else {
			log.Printf("access token refreshed!  expires: %s\n", transport.Token.Expiry)
		}
	}
}
