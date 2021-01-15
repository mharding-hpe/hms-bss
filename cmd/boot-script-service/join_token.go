// Copyright 2020 Hewlett Packard Enterprise Development LP
//
// Retrieve a join token used by a SPIRE agent to connect to a SPIRE server.
//
// NOTE:
//$ SP=https://spire-tokens.spire:54440/api
//$ curl -k $SP
//{"version":"0.2.1"}
//$ curl -k -d xname=x1000c0s0b0n0 $SP/token
//{"join_token":"aecbbf2b-14e5-4e2e-a7b2-864f05a49d0b"}

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type spireRespType struct {
	Title     string `json:"title,omitempty"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	JoinToken string `json:"join_token,omitempty"`
}

var (
	spireTokenClient   *http.Client // Spire Token Service client
	spireTokensBaseURL string
)

func spireTokenServiceInit(urlBase, opts string) error {
	u, err := url.Parse(urlBase)
	if err != nil {
		return fmt.Errorf("URL parse error %s, URL: %s", err, urlBase)
	}
	https := u.Scheme == "https"
	insecure := false
	for _, opt := range strings.Split(opts, ",") {
		if strings.ToLower(opt) == "insecure" {
			insecure = true
			break
		}
	}
	spireTokenClient = new(http.Client)
	if https && insecure {
		tcfg := new(tls.Config)
		tcfg.InsecureSkipVerify = true
		trans := new(http.Transport)
		trans.TLSClientConfig = tcfg
		spireTokenClient.Transport = trans
		log.Printf("WARNING: insecure https connection to spire token service\n")
	}
	return nil
}

func getJoinToken(xname string) (string, error) {
	url := spireTokensBaseURL + "/api/token"
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer([]byte("xname="+xname)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Close = true
	rsp, err := spireTokenClient.Do(req)
	if err != nil {
		log.Printf("ERROR: %s: sending request to spire token service: %s", url, err)
		return "", err
	}
	if rsp.StatusCode != http.StatusOK && rsp.StatusCode != http.StatusCreated && rsp.StatusCode != http.StatusAccepted {
		log.Printf("ERROR: %s: spire token service response for %s: %s", url, xname, rsp.Status)
	}
	rspBody, err := ioutil.ReadAll(rsp.Body)
	debugf("Join Token Service response body: '%s'", rspBody)
	if err != nil {
		log.Printf("ERROR: %s: reading response from spire token service: %s", url, err)
		return "", err
	}
	rsp.Body.Close()

	var spireResp spireRespType
	err = json.Unmarshal(rspBody, &spireResp)
	debugf("json.Unmarshal('%s', &spireResp): %v", rspBody, spireResp)
	if err != nil {
		log.Printf("ERROR: %s: unmarshalling spire token service response: %s", url, err)
		return "", err
	} else if spireResp.JoinToken == "" {
		if spireResp.Title != "" || spireResp.Detail != "" {
			err = fmt.Errorf("ERROR: %s: Join token retrieval failed: %s: %s", url, spireResp.Title, spireResp.Detail)
		} else {
			err = fmt.Errorf("ERROR: %s: Did not receive join token: %s", url, rspBody)
		}
		log.Printf("%s", err)
	}
	return spireResp.JoinToken, err
}