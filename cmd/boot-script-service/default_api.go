// Copyright 2018-2020 Hewlett Packard Enterprise Development LP
/*
 * Boot Script Server
 *
 * The boot script server will collect all information required to produce an
 * iPXE boot script for each node of a system.  This script will the be
 * generated on demand and delivered to the requesting node during an iPXE
 * boot.  The main items the script will deliver are the kernel image URL/path,
 * boot arguments, and the initrd URL/path.  Note that the kernel and initrd
 * images are specified with a URL or path.  A plain path will result in a tfpt
 * download from this server.  If a URL is provided, it can be from any
 * available service which iPXE supports, and any location that the iPXE client
 * has access to. It is not restricted to a particular Cray provided service.
 *
 * API version: 1.0.0
 * Generated by: Swagger Codegen (https://github.com/swagger-api/swagger-codegen.git)
 */

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	base "stash.us.cray.com/HMS/hms-base"
	"stash.us.cray.com/HMS/hms-bss/pkg/bssTypes"
	hms_s3 "stash.us.cray.com/HMS/hms-s3"
	"strconv"
	"strings"
	"time"
)

const (
	bootDataBasePath = "/bootdata/"
	unknownPrefix    = "Unknown-"
	joinTokenVarName = "SPIRE_JOIN_TOKEN"
)

var blockedRoles []string

// Figure out what server the ipxe boot scripts should reference when chaining
// to new BSS requests.  This is normally the API gateway.  Allow this to be
// overridden with the BSS_IPXE_SERVER environment variable.
var ipxeServer = getEnvVal("BSS_IPXE_SERVER", "api-gw-service-nmn.local")
var chainProto = getEnvVal("BSS_CHAIN_PROTO", "https")
var gwURI = getEnvVal("BSS_GW_URI", "/apis/bss")

// Store ptr to S3 client
var s3Client *hms_s3.S3Client

type scriptParams struct {
	xname string
	nid   string
}

// Note that we allow an empty string if the env variable is defined as such.
func getEnvVal(envVar, defVal string) string {
	if e, ok := os.LookupEnv(envVar); ok {
		return e
	}
	return defVal
}

func checkURL(u string) (string, error) {
	p, err := url.Parse(u)
	if err != nil || !strings.EqualFold(p.Scheme, "s3") {
		return u, nil
	}
	// This is an S3 "url".  The way we are using them are that the "host" part
	// of the URL is the bucket, and the rest is the key.  If the "host" is
	// nil, then we will use the first part of the path as the bucket.
	if err != nil {
		return "", err
	}
	bucket := ""
	key := ""
	if p.Host == "" {
		tmp := strings.Split(strings.Trim(p.Path, "/"), "/")
		bucket = tmp[0]
		key = strings.Join(tmp[1:], "/")
	} else {
		bucket = p.Host
		key = p.Path
	}
	if s3Client == nil {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		httpClient := &http.Client{Transport: tr}
		info, err := hms_s3.LoadConnectionInfoFromEnvVars()
		info.Bucket = bucket
		if err != nil {
			log.Printf("Failed to load S3 connection info: %s", err)
		}
		s3Client, err = hms_s3.NewS3Client(info, httpClient)
	} else {
		s3Client.SetBucket(bucket)
	}
	if s3Client != nil {
		return s3Client.GetURL(key, 24*time.Hour)
	}
	return "", err
}

func BootparametersGetAll(w http.ResponseWriter, r *http.Request) {
	var results []bssTypes.BootParams
	for _, image := range GetKernelInfo() {
		var bp bssTypes.BootParams
		bp.Params = image.Params
		bp.Kernel = image.Path
		results = append(results, bp)
	}
	for _, image := range GetInitrdInfo() {
		var bp bssTypes.BootParams
		bp.Params = image.Params
		bp.Initrd = image.Path
		results = append(results, bp)
	}
	var names []string
	if kvl, e := getTags(); e == nil {
		for _, x := range kvl {
			name := extractParamName(x)
			names = append(names, name)
			var bds BootDataStore
			e = json.Unmarshal([]byte(x.Value), &bds)
			if e == nil {
				bd := bdConvert(bds)
				var bp bssTypes.BootParams
				bp.Hosts = append(bp.Hosts, name)
				bp.Params = bd.Params
				bp.Kernel = bd.Kernel.Path
				bp.Initrd = bd.Initrd.Path
				bp.CloudInit = bd.CloudInit
				results = append(results, bp)
			}
		}
	}
	debugf("Retreived names: %v", names)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(results)
	if err != nil {
		log.Printf("Yikes, I couldn't encode a JSON status response: %s\n", err)
	}
}

func BootparametersGet(w http.ResponseWriter, r *http.Request) {
	debugf("BootparametersGet(): Received request %v\n", r.URL)
	var args bssTypes.BootParams
	debugf("Ready to decode %v\n", r.Body)
	p, err := ioutil.ReadAll(r.Body)
	if err != nil {
		// Some error occurred while retreiving the body, return an error
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Failed to receive request body: %v", err))

		return
	}
	r.ParseForm() // r.Form is empty until after parsing
	mac := strings.Join(r.Form["mac"], ",")
	name := strings.Join(r.Form["name"], ",")
	nid := strings.Join(r.Form["nid"], ",")
	qparams := mac != "" || name != "" || nid != ""

	if len(p) == 0 && !qparams {
		// No body sent, so send all the boot parameters
		BootparametersGetAll(w, r)
		return
	}
	err = json.Unmarshal(p, &args)
	if err != nil && !qparams {
		// Some problem with the request
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Failed to interpret request body '%s': %v", p, err))
		return
	}
	if mac != "" {
		args.Macs = append(args.Macs, strings.Split(mac, ",")...)
	}
	if name != "" {
		args.Hosts = append(args.Hosts, strings.Split(name, ",")...)
	}
	if nid != "" {
		for _, n := range strings.Split(nid, ",") {
			tmp, err := strconv.ParseInt(n, 0, 0)
			if err != nil {
				// Deal with conversion error.
				base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
					fmt.Sprintf("Bad Request - Invalid nid '%s'", n))
				return
			} else {
				args.Nids = append(args.Nids, int32(tmp))
			}
		}
	}

	debugf("Received boot parameters: %v\n", args)
	var results []bssTypes.BootParams
	if args.Kernel != "" || args.Initrd != "" {
		for _, image := range GetKernelInfo() {
			if image.Path == args.Kernel {
				var bp bssTypes.BootParams
				bp.Params = image.Params
				bp.Kernel = image.Path
				results = append(results, bp)
			}
		}
		for _, image := range GetInitrdInfo() {
			if image.Path == args.Initrd {
				var bp bssTypes.BootParams
				bp.Params = image.Params
				bp.Initrd = image.Path
				results = append(results, bp)
			}
		}
	}
	names := GetNames()
	for _, name := range names {
		bd, smc := LookupByName(name)
		debugf("Found %s: %v | %v\n", name, bd, smc)
		var bp bssTypes.BootParams
		ok := false
		for _, v := range args.Hosts {
			if v == smc.ID || v == smc.Fqdn || v == name {
				ok = true
				break
			}
		}
		if !ok {
		Outer:
			for _, v := range args.Macs {
				for _, m := range smc.Mac {
					if strings.EqualFold(v, m) {
						ok = true
						break Outer
					}
				}
			}
		}
		if !ok {
			for _, v := range args.Nids {
				if nid, err := smc.NID.Int64(); err == nil && int64(v) == nid {
					ok = true
					break
				}
			}
		}
		if ok {
			bp.Hosts = append(bp.Hosts, name)
			bp.Params = bd.Params
			bp.Kernel = bd.Kernel.Path
			bp.Initrd = bd.Initrd.Path
			bp.CloudInit = bd.CloudInit
			results = append(results, bp)
		}
	}
	if results == nil {
		// Could not find any boot parameters.  Set up error message.
		// We want the error message to reflect the request.
		var objs []string
		if len(args.Hosts) > 0 {
			objs = append(objs, "Hosts")
		}
		if len(args.Macs) > 0 {
			objs = append(objs, "MACs")
		}
		if len(args.Nids) > 0 {
			objs = append(objs, "NIDs")
		}
		if args.Kernel != "" {
			objs = append(objs, "kernel")
		}
		if args.Initrd != "" {
			objs = append(objs, "initrd")
		}
		l := len(objs)
		if l == 0 {
			// Nothing was requested, so this is a bad request
			base.SendProblemDetailsGeneric(w, http.StatusBadRequest, "No specified data requested")
		} else {
			msg := "Cannot find boot parameters for requested " +
				strings.Join(objs[:l-1], ", ")
			if l > 1 {
				msg += " or "
			}
			msg += objs[l-1]
			base.SendProblemDetailsGeneric(w, http.StatusNotFound, msg)
		}
		return
	}
	debugf("Retreived names: %v", names)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	err = json.NewEncoder(w).Encode(results)
	if err != nil {
		log.Printf("Yikes, I couldn't encode a JSON status response: %s\n", err)
	}
}

func LogBootParameters(prefix string, v interface{}) {
	j, e := json.MarshalIndent(v, "", "  ")
	if e == nil {
		log.Printf("%s: %s", prefix, j)
	} else {
		log.Printf("%s: %v", prefix, v)
	}
}

func BootparametersPost(w http.ResponseWriter, r *http.Request) {
	debugf("BootparametersPost(): Received request %v\n", r.URL)
	var args bssTypes.BootParams
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&args)
	if err != nil {
		debugf("BootparametersPost: Bad Request: %v\n", err)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Bad Request: %s", err))
		return
	}
	debugf("Received boot parameters: %v\n", args)
	err = StoreNew(args)
	if err == nil {
		LogBootParameters("/bootparameters POST", args)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusCreated)
	} else {
		LogBootParameters(fmt.Sprintf("/bootparameters POST FAILED: %s", err.Error()), args)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Bad Request: %s", err))
	}
}

func BootparametersPut(w http.ResponseWriter, r *http.Request) {
	debugf("BootparametersPut(): Received request %v\n", r.URL)
	var args bssTypes.BootParams
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&args)
	if err != nil {
		debugf("BootparametersPut: Bad Request: %v\n", err)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Bad Request: %s", err))
		return
	}
	debugf("Received boot parameters: %v\n", args)
	err = Store(args)
	if err == nil {
		LogBootParameters("/bootparameters PUT", args)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
	} else {
		LogBootParameters(fmt.Sprintf("/bootparameters PATCH FAILED: %s", err.Error()), args)
		herr, ok := base.GetHMSError(err)
		if ok && herr.GetProblem() != nil {
			base.SendProblemDetails(w, herr.GetProblem(), 0)
		} else {
			base.SendProblemDetailsGeneric(w, http.StatusBadRequest, "No data")
		}
	}
}

func BootparametersPatch(w http.ResponseWriter, r *http.Request) {
	debugf("BootparametersPatch(): Received request %v\n", r.URL)
	var args bssTypes.BootParams
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&args)
	if err != nil {
		debugf("BootparametersPatch: Bad Request: %v\n", err)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Bad Request: %s", err))
		return
	}
	debugf("Received boot parameters: %v\n", args)
	err = Update(args)
	if err != nil {
		LogBootParameters(fmt.Sprintf("/bootparameters PATCH FAILED: %s", err.Error()), args)
		base.SendProblemDetailsGeneric(w, http.StatusNotFound,
			fmt.Sprintf("Not Found: %s", err))
	} else {
		LogBootParameters("/bootparameters PATCH", args)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
	}
}

func BootparametersDelete(w http.ResponseWriter, r *http.Request) {
	debugf("BootParametersDelete(): Received request %v\n", r.URL)
	var args bssTypes.BootParams
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&args)
	if err != nil {
		debugf("BootparametersDelete: Bad Request: %v\n", err)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
			fmt.Sprintf("Bad Request: %s", err))
		return
	}
	if err == nil {
		err = Remove(args)
	}
	if err != nil {
		LogBootParameters(fmt.Sprintf("/bootparameters DELETE FAILED: %s", err.Error()), args)
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest, err.Error())
	} else {
		LogBootParameters("/bootparameters DELETE", args)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
	}
}

func getIntParam(r *http.Request, param string, def int64) (int64, error) {
	str := strings.Join(r.Form[param], "")
	ret := def
	var err error
	if str != "" {
		var tmp int64
		tmp, err = strconv.ParseInt(str, 0, 0)
		if err == nil {
			ret = tmp
		}
	}
	return ret, err
}

// Function paramExists checks for a specific boot parameter to see if it exists
// in the current boot parameters.
func paramExists(params, pname string) bool {
	for _, s := range strings.Split(params, " ") {
		if strings.HasPrefix(s, pname) {
			return true
		}
	}
	return false
}

// Function checkParam() scans the current parameter string looking for a
// parameter pname.  If this parameter does not exist, the it is added to the
// parameter string with the value pval.  If the parameter is already found in
// the parameter string, then it is unchanged.  The resultant parameter string
// is returned.
func checkParam(params, pname, pval string) string {
	debugf("checkParam(\"%s\", \"%s\", \"%s\")\n", params, pname, pval)
	if pval != "" && !paramExists(params, pname) {
		params += " " + pname + pval
	}
	debugf("checkParam returning \"%s\"\n", params)
	return params
}

type paramValRetreiver func() (string, error)

func paramSubstitute(params, pvar string, getVal paramValRetreiver) (string, error) {
	if pvar[0:1] != "${" {
		pvar = "${" + pvar + "}"
	}
	var err error
	if strings.Index(params, pvar) >= 0 {
		// The variable exists, so we need to do the substitution.
		var val string
		val, err = getVal()
		// The getVal() function is expected to log the errors.  We check the
		// err response in order to determine if the value returned is valid.
		// We only do the substitution if the response was valid.
		if err == nil {
			params = strings.Replace(params, pvar, val, -1)
		}
	}
	return params, err
}

// Function buildBootScript will construct the iPXE boot script based on the
// BootData and additional parameters provided.  The resultant script is
// returned as a string.  If an error occurs, a null string is returned along
// with the error.
func buildBootScript(bd BootData, sp scriptParams, chain, descr string) (string, error) {
	debugf("buildBootScript(%v, %v, %v, %v)\n", bd, sp, chain, descr)
	if bd.Kernel.Path == "" {
		return "", fmt.Errorf("%s: this host not configured for booting.", descr)
	}

	params := bd.Params
	if bd.Kernel.Params != "" {
		params += " " + bd.Kernel.Params
	}
	if bd.Initrd.Params != "" {
		params += " " + bd.Initrd.Params
	}

	// Check for special boot parameters.
	params = checkParam(params, "xname=", sp.xname)
	params = checkParam(params, "nid=", sp.nid)
	// Inject the cloud init address info into the kernal params. If the target
	// image does not have cloud-init enabled this wont hurt anything.
	// If it does, it tells it to come back to us for the cloud-init meta-data
	params = checkParam(params, "ds=", fmt.Sprintf("nocloud-net;s=%s/", advertiseAddress))

	var err error
	params, err = paramSubstitute(params, joinTokenVarName,
		func() (string, error) { return getJoinToken(sp.xname) })

	if err != nil {
		return "", err
	}

	script := "#!ipxe\n"
	if bd.Initrd.Path != "" {
		start := strings.Index(params, "initrd")
		if start != -1 {
			end := start
			for string(params[end]) != " " {
				end++
			}
			params = params[:start] + params[end:]
		}
		params = "initrd=initrd " + params
	}
	u := bd.Kernel.Path
	u, err = checkURL(u)
	if err == nil {
		script += "kernel --name kernel " + u + " " + strings.Trim(params, " ")
		script += " || goto boot_retry\n"
		if bd.Initrd.Path != "" {
			u, err = checkURL(bd.Initrd.Path)
			if err == nil {
				script += "initrd --name initrd " + u + " || goto boot_retry\n"
			}
		}
		script += "boot || goto boot_retry\n:boot_retry\n"
		// We could vary the length of the sleep based on retry count or some
		// other criteria.
		// For now, just sleep a bit
		script += fmt.Sprintf("sleep %d\n", retryDelay) + chain + "\n"
	}
	return script, err
}

// Function unknownBootScript() constructs the boot script for an unknown host
// or unknown MAC address.  This is done based on the system architecture.  If
// the architecture is unknown, the returned script is simply a chained request
// which will allow the requesting node to return the architecture.
func unknownBootScript(arch, mac, name string, nid int, ts int64, descr string) (string, bool, error) {
	debugf("unknownBootScript(%s)", arch)
	var script string
	var err error
	chain := "chain " + chainProto + "://" + ipxeServer + gwURI + "/boot/v1/bootscript"
	if mac != "" {
		chain += "?mac=" + mac
	} else if name != "" {
		chain += "?name=" + name
	} else if nid >= 0 {
		chain += fmt.Sprintf("?nid=%d", nid)
	} else {
		chain += "?mac=${net/net0}" // FIXME: What should this be????
	}
	chain += fmt.Sprintf("&arch=${buildarch}&ts=%d", ts)
	debugf("ts: %d, smTimeStamp: %d", ts, smTimeStamp)
	retrievingState := checkState(arch == "")
	if retrievingState {
		// Either request the architecture or delay for HSM retrieval
		script = "#!ipxe\n"
		if retrievingState {
			// Our state was out of date and is in the process of being updated.
			// In order to prevent iPXE from the requester timing out, we will
			// send it a chained request with a delay.  It will make a new
			// request after a delay, at which point we should have new state
			// data.  If retrieving state takes longer than our delay, when the
			// next request comes in, it will wait for the lock to clear, at
			// which point the updated state will be there.
			script += fmt.Sprintf("sleep %d\n", hsmRetrievalDelay)
		} else if ukeys, e := unknownKeys(); e != nil || len(ukeys) == 0 {
			err = fmt.Errorf("%s: no configuration available for unknown hosts", descr)
			log.Printf("%s: no configuration available for unknown hosts", descr)
		} else {
			log.Printf("%s: requesting architecture of unknown host", descr)
		}
		script += chain + "\n"
	} else {
		bd := lookup(unknownPrefix+arch, "", "", "")
		script, err = buildBootScript(bd, scriptParams{}, chain, descr)
	}
	return script, retrievingState, err
}

// Function blacklist() determines if this node is supposed to be blacklisted,
// meaning we do not return a bootscript.  As the criteria for blacklisting
// may change over time, we isolate this code to a separate function.  An error
// is returned if the node is blacklisted.  Returning nil indicates the node
// should receive the boot script.  We will not blacklist a node if it has a
// boot configuration for itself specifically, or if its role has a specific
// configuration.
func blacklist(comp SMComponent) (err error) {
	block := false
	for _, r := range blockedRoles {
		if strings.EqualFold(r, comp.Role) {
			block = true
			break
		}
	}
	if block {
		checkHost := func(x string) error { _, e := lookupHost(x); return e }
		// This node is a candidate to be blacklisted. So we need to see
		// if it has a configuration specifically for itself.  If so, we
		// will still serve it.
		if checkHost(comp.ID) != nil && (comp.Role == "" || checkHost(comp.Role) != nil) {
			err = fmt.Errorf("Node %s blocked, role: %s", comp.ID, comp.Role)
		}
	}
	return
}

func BootscriptGet(w http.ResponseWriter, r *http.Request) {
	debugf("BootscriptGet(): Received request %v\n", r.URL)

	r.ParseForm() // r.Form is empty until after parsing
	mac := strings.Join(r.Form["mac"], "")
	name := strings.Join(r.Form["name"], "")
	arch := strings.Join(r.Form["arch"], "")

	tmp_nid, _ := getIntParam(r, "nid", -1)
	tmp_retry, _ := getIntParam(r, "retry", 0)
	ts, _ := getIntParam(r, "ts", time.Now().Unix())

	nid := int(tmp_nid)
	retry := int(tmp_retry)

	var bd BootData
	var comp SMComponent
	var descr string

	if mac != "" {
		bd, comp = LookupByMAC(mac)
		descr = fmt.Sprintf("MAC %s", mac)
		if comp.ID != "" {
			descr += fmt.Sprintf(" (%s)", comp.ID)
		}
	} else if name != "" {
		bd, comp = LookupByName(name)
		descr = name
		if comp.ID != "" && comp.ID != name {
			descr += fmt.Sprintf(" (%s)", comp.ID)
		}
	} else if nid >= 0 {
		bd, comp = LookupByNid(nid)
		descr = fmt.Sprintf("NID %d", nid)
		if comp.ID != "" {
			descr += fmt.Sprintf(" (%s)", comp.ID)
		}
	} else {
		base.SendProblemDetailsGeneric(w, http.StatusBadRequest, "Need a mac=, name=, or nid= parameter")
		log.Printf("BSS request failed: bootscript request without mac=, name=, or nid= parameter")
		return
	}

	debugf("bd: %v\n", bd)
	debugf("comp: %v\n", comp)

	var script string
	var err error

	// Check if this is a node in the discovery process.  We assume this if the
	// node is not yet known, or if the node is not configured for booting.  In
	// either of these cases, we want to boot the discovery kernel.
	unknown := comp.ID == "" || !comp.EndpointEnabled || bd.Kernel.Path == ""
	retreivingState := false
	if unknown {
		debugf("Unknown: comp: %v", comp)
		if name == "" {
			name = comp.ID
		}
		if comp.ID != "" {
			if n, e := comp.NID.Int64(); e == nil {
				nid = int(n)
			}
		}
		if mac == "" && comp.Mac != nil {
			for _, m := range comp.Mac {
				if m != "" {
					// Sometimes we see an empty string in the list of MAC addresses!
					mac = m
					break
				}
			}
		}
		debugf("Unknown/disabled node, ID: '%s', name = %s, mac = %s, nid = %d", comp.ID, name, mac, nid)
		descr = "Unknown " + descr
		if arch != "" {
			descr += " architecture " + arch
		}
		script, retreivingState, err = unknownBootScript(arch, mac, name, nid, ts, descr)
		if err != nil {
			debugf("unknownBootScript returned error: %s", err.Error())
		}
	}
	if !unknown || (unknown && err != nil && comp.ID != "") {
		// We wanted to boot the discovery kernel, but were unable to.  This
		// happens when there is no discovery kernel configured.  If this is
		// a known component, we will then attempt to provide a non-discovery
		// bootscript.
		err = blacklist(comp)
		if err == nil {
			if mac == "" && comp.Mac != nil {
				mac = comp.Mac[0]
			}
			sp := scriptParams{comp.ID, comp.NID.String()}
			chain := "chain " + chainProto + "://" + ipxeServer + gwURI + r.URL.Path
			if mac != "" {
				chain += "?mac=" + mac
			} else {
				chain += "?name=" + comp.ID
			}
			chain += fmt.Sprintf("&retry=%d", retry+1)
			retreivingState = checkState(false)
			if retreivingState {
				// We want to respond with a delayed chain response so that the
				// node will retry in a bit after we have updated our state info
				script = "#!ipxe\nsleep 10\n" + chain + "\n"
			} else {
				script, err = buildBootScript(bd, sp, chain, descr)
			}
		}
	}
	if err == nil {
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, err = fmt.Fprintf(w, "%s\n", script)
		if err == nil {
			if retreivingState {
				log.Printf("BSS request delayed for %s while updating state", descr)
			} else {
				log.Printf("BSS request succeeded for %s", descr)
			}
		} else {
			log.Printf("BSS request failed writing response for %s: %s", descr, err.Error())
		}
	} else {
		base.SendProblemDetailsGeneric(w, http.StatusNotFound, err.Error())
		if strings.HasPrefix(err.Error(), descr) {
			log.Printf("BSS request failed: %s", err.Error())
		} else {
			log.Printf("BSS request failed for %s: %s", descr, err.Error())
		}
	}
}

func HostsGet(w http.ResponseWriter, r *http.Request) {
	debugf("HostsGet(): Received request %v\n", r.URL)
	r.ParseForm() // r.Form is empty until after parsing
	mac := strings.Join(r.Form["mac"], ",")
	name := strings.Join(r.Form["name"], ",")
	nid := strings.Join(r.Form["nid"], ",")
	qparams := mac != "" || name != "" || nid != ""
	state := getState()
	results := state.Components
	if qparams {
		results = nil
		if name != "" {
			for _, n := range strings.Split(name, ",") {
				comp, ok := FindSMCompByName(n)
				if ok {
					results = append(results, comp)
				} else {
					base.SendProblemDetailsGeneric(w, http.StatusNotFound,
						fmt.Sprintf("Not Found - Unknown host name '%s'", n))
					return
				}
			}
		}
		if mac != "" {
			for _, m := range strings.Split(mac, ",") {
				comp, ok := FindSMCompByMAC(m)
				if ok {
					results = append(results, comp)
				} else {
					base.SendProblemDetailsGeneric(w, http.StatusNotFound,
						fmt.Sprintf("Not Found - Unknown MAC address '%s'", m))
					return
				}
			}
		}
		if nid != "" {
			for _, n := range strings.Split(nid, ",") {
				tmp, err := strconv.ParseInt(n, 0, 0)
				if err != nil {
					base.SendProblemDetailsGeneric(w, http.StatusBadRequest,
						fmt.Sprintf("Bad Request - Invalid nid '%s'", n))
					return
				}
				comp, ok := FindSMCompByNid(int(tmp))
				if ok {
					results = append(results, comp)
				} else {
					base.SendProblemDetailsGeneric(w, http.StatusNotFound,
						fmt.Sprintf("Not Found - Unknown NID '%s'", n))
					return
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(results)
	if err != nil {
		log.Printf("Yikes, I couldn't encode '%v' as a JSON status response: %s\n", results, err)
	}
}

func HostsPost(w http.ResponseWriter, r *http.Request) {
	debugf("HostsPost(): Received request %v\n", r.URL)
	refreshState(time.Now().Unix())
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNoContent)
}

func DumpstateGet(w http.ResponseWriter, r *http.Request) {
	type State struct {
		Components []SMComponent         `json:"Components"`
		Params     []bssTypes.BootParams `json:"Params"`
	}
	debugf("DumpstateGet(): Received request %v\n", r.URL)
	var results State
	state := getState()
	results.Components = state.Components
	for _, image := range GetKernelInfo() {
		var bp bssTypes.BootParams
		bp.Params = image.Params
		bp.Kernel = image.Path
		results.Params = append(results.Params, bp)
	}
	for _, image := range GetInitrdInfo() {
		var bp bssTypes.BootParams
		bp.Params = image.Params
		bp.Initrd = image.Path
		results.Params = append(results.Params, bp)
	}

	kvl, err := getTags()
	var names []string
	if err == nil {
		for _, x := range kvl {
			name := extractParamName(x)
			names = append(names, name)
			var bds BootDataStore
			if e := json.Unmarshal([]byte(x.Value), &bds); e == nil {
				bd := bdConvert(bds)
				var bp bssTypes.BootParams
				bp.Hosts = append(bp.Hosts, name)
				bp.Params = bd.Params
				bp.Kernel = bd.Kernel.Path
				bp.Initrd = bd.Initrd.Path
				results.Params = append(results.Params, bp)
			}
		}
	}
	debugf("Retreived names: %v", names)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	err = json.NewEncoder(w).Encode(results)
	if err != nil {
		log.Printf("Yikes, I couldn't encode '%v' as a JSON status response: %s\n", results, err)
	}
}