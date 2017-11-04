// Copyright (c) 2017 Zededa, Inc.
// All rights reserved.

// Process input changes from a config directory containing json encoded files
// with VerifyImageConfig and compare against VerifyImageStatus in the status
// dir.
// Move the file from downloads/pending/<claimedsha>/<safename> to
// to downloads/verifier/<claimedsha>/<safename> and make RO, then attempt to
// verify sum.
// Once sum is verified, move to downloads/verified/<sha>/<filename> where
// the filename is the last part of the URL (after the last '/')
// Note that different URLs for same file will download to the same <sha>
// directory. We delete duplicates assuming the file content will be the same.

// XXX TBD add a signature on the checksum. Verify against root CA.

// XXX TBD separately add support for verifying the signatures on the meta-data (the AIC)

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/zededa/go-provision/types"
	"github.com/zededa/go-provision/watch"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"
)

var imgCatalogDirname string

func main() {
	log.Printf("Starting verifier\n")
	watch.CleanupRestarted("verifier")

	// Keeping status in /var/run to be clean after a crash/reboot
	baseDirname := "/var/tmp/verifier"
	runDirname := "/var/run/verifier"
	configDirname := baseDirname + "/config"
	statusDirname := runDirname + "/status"
	imgCatalogDirname = "/var/tmp/zedmanager/downloads"
	pendingDirname := imgCatalogDirname + "/pending"
	verifierDirname := imgCatalogDirname + "/verifier"
	verifiedDirname := imgCatalogDirname + "/verified"

	if _, err := os.Stat(baseDirname); err != nil {
		if err := os.Mkdir(baseDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := os.Stat(configDirname); err != nil {
		if err := os.Mkdir(configDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := os.Stat(runDirname); err != nil {
		if err := os.Mkdir(runDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := os.Stat(statusDirname); err != nil {
		if err := os.Mkdir(statusDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	// Don't remove directory since there is a watch on it
	locations, err := ioutil.ReadDir(statusDirname)
	if err != nil {
		log.Fatal(err)
	}
	for _, location := range locations {
		filename := statusDirname + "/" + location.Name()
		if err := os.RemoveAll(filename); err != nil {
			log.Fatal(err)
		}
	}

	if _, err := os.Stat(imgCatalogDirname); err != nil {
		if err := os.MkdirAll(imgCatalogDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}

	if _, err := os.Stat(pendingDirname); err != nil {
		if err := os.Mkdir(pendingDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	// Remove any files which didn't make it past the verifier
	if err := os.RemoveAll(verifierDirname); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(verifierDirname); err != nil {
		if err := os.Mkdir(verifierDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := os.Stat(verifiedDirname); err != nil {
		if err := os.Mkdir(verifiedDirname, 0700); err != nil {
			log.Fatal(err)
		}
	}

	// Creates statusDir entries for already verified files
	handleInit(verifiedDirname, statusDirname, "")

	fileChanges := make(chan string)
	go watch.WatchConfigStatusAllowInitialConfig(configDirname,
		statusDirname, fileChanges)
	for {
		change := <-fileChanges
		watch.HandleConfigStatusEvent(change,
			configDirname, statusDirname,
			&types.VerifyImageConfig{},
			&types.VerifyImageStatus{},
			handleCreate, handleModify, handleDelete, nil)
	}
}

// Determine which files we have already verified and set status for them
func handleInit(verifiedDirname string, statusDirname string,
	parentDirname string) {
	fmt.Printf("handleInit(%s, %s, %s)\n",
		verifiedDirname, statusDirname, parentDirname)
	locations, err := ioutil.ReadDir(verifiedDirname)
	if err != nil {
		log.Fatal(err)
	}
	for _, location := range locations {
		filename := verifiedDirname + "/" + location.Name()
		fmt.Printf("handleInit: Looking in %s\n", filename)
		if location.IsDir() {
			handleInit(filename, statusDirname, location.Name())
		} else {
			status := types.VerifyImageStatus{
				Safename:    location.Name(),
				ImageSha256: parentDirname,
				State:       types.DELIVERED,
			}
			writeVerifyImageStatus(&status,
				statusDirname+"/"+location.Name()+".json")
		}
	}
	fmt.Printf("handleInit done for %s, %s, %s\n",
		verifiedDirname, statusDirname, parentDirname)
	// Report to zedmanager that init is done
	watch.SignalRestarted("verifier")
}

func writeVerifyImageStatus(status *types.VerifyImageStatus,
	statusFilename string) {
	b, err := json.Marshal(status)
	if err != nil {
		log.Fatal(err, "json Marshal VerifyImageStatus")
	}
	// We assume a /var/run path hence we don't need to worry about
	// partial writes/empty files due to a kernel crash.
	err = ioutil.WriteFile(statusFilename, b, 0644)
	if err != nil {
		log.Fatal(err, statusFilename)
	}
}

func handleCreate(statusFilename string, configArg interface{}) {
	var config *types.VerifyImageConfig

	switch configArg.(type) {
	default:
		log.Fatal("Can only handle VerifyImageConfig")
	case *types.VerifyImageConfig:
		config = configArg.(*types.VerifyImageConfig)
	}
	log.Printf("handleCreate(%v) for %s\n",
		config.Safename, config.DownloadURL)
	// Start by marking with PendingAdd
	status := types.VerifyImageStatus{
		Safename:    config.Safename,
		ImageSha256: config.ImageSha256,
		PendingAdd:  true,
		State:       types.DOWNLOADED,
		RefCount:    config.RefCount,
	}
	writeVerifyImageStatus(&status, statusFilename)

	// Form the unique filename in /var/tmp/zedmanager/downloads/pending/
	// based on the claimed Sha256 and safename, and the same name
	// in downloads/verifier/. Form a shorter name for
	// downloads/verified/.
	pendingDirname := imgCatalogDirname + "/pending/" + config.ImageSha256
	pendingFilename := pendingDirname + "/" + config.Safename
	verifierDirname := imgCatalogDirname + "/verifier/" + config.ImageSha256
	verifierFilename := verifierDirname + "/" + config.Safename

	// Check if the verified result already exists; if so we're done
	// XXX no, because can't get a config until the file is downloaded
	// Instead push Status based on the initial content? But want
	// a config with a refcount.

	// Move to verifier directory which is RO
	// XXX should have dom0 do this and/or have RO mounts
	fmt.Printf("Move from %s to %s\n", pendingFilename, verifierFilename)
	if _, err := os.Stat(pendingFilename); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(verifierDirname); err == nil {
		if err := os.RemoveAll(verifierDirname); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.MkdirAll(verifierDirname, 0700); err != nil {
		log.Fatal(err)
	}

	if err := os.Rename(pendingFilename, verifierFilename); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(verifierDirname, 0500); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(verifierFilename, 0400); err != nil {
		log.Fatal(err)
	}
	// Clean up empty directory
	if err := os.Remove(pendingDirname); err != nil {
		log.Fatal(err)
	}
	log.Printf("Verifying URL %s file %s\n",
		config.DownloadURL, verifierFilename)

	f, err := os.Open(verifierFilename)
	if err != nil {
		status.LastErr = fmt.Sprintf("%v", err)
		status.LastErrTime = time.Now()
		status.State = types.INITIAL
		writeVerifyImageStatus(&status, statusFilename)
		log.Printf("handleCreate failed for %s\n", config.DownloadURL)
		return
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		status.LastErr = fmt.Sprintf("%v", err)
		status.LastErrTime = time.Now()
		status.State = types.INITIAL
		writeVerifyImageStatus(&status, statusFilename)
		log.Printf("handleCreate failed for %s\n", config.DownloadURL)
		return
	}
	f.Close()

	got := fmt.Sprintf("%x", h.Sum(nil))
	if got != config.ImageSha256 {
		fmt.Printf("got      %s\n", got)
		fmt.Printf("expected %s\n", config.ImageSha256)
		status.LastErr = fmt.Sprintf("got %s expected %s",
			got, config.ImageSha256)
		status.LastErrTime = time.Now()
		status.PendingAdd = false
		status.State = types.INITIAL
		writeVerifyImageStatus(&status, statusFilename)
		log.Printf("handleCreate failed for %s\n", config.DownloadURL)
		return
	}
	// Move directory from downloads/verifier to downloads/verified
	// XXX should have dom0 do this and/or have RO mounts
	finalDirname := imgCatalogDirname + "/verified/" + config.ImageSha256
	filename := safenameToFilename(config.Safename)
	finalFilename := finalDirname + "/" + filename
	fmt.Printf("Move from %s to %s\n", verifierFilename, finalFilename)
	if _, err := os.Stat(verifierFilename); err != nil {
		log.Fatal(err)
	}
	// XXX change log.Fatal to something else?
	if _, err := os.Stat(finalDirname); err == nil {
		// Directory exists thus we have a sha256 collision presumably
		// due to multiple safenames (i.e., URLs) for the same content.
		// Delete existing to avoid wasting space.
		locations, err := ioutil.ReadDir(finalDirname)
		if err != nil {
			log.Fatal(err)
		}
		for _, location := range locations {
			log.Printf("Identical sha256 (%s) for safenames %s and %s; deleting old\n",
				config.ImageSha256, location.Name(),
				config.Safename)
		}

		if err := os.RemoveAll(finalDirname); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.Mkdir(finalDirname, 0700); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(verifierFilename, finalFilename); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(finalDirname, 0500); err != nil {
		log.Fatal(err)
	}
	// Clean up empty directory
	if err := os.Remove(verifierDirname); err != nil {
		log.Fatal(err)
	}

	status.PendingAdd = false
	status.State = types.DELIVERED
	writeVerifyImageStatus(&status, statusFilename)
	log.Printf("handleCreate done for %s\n", config.DownloadURL)
}

// Remove initial part up to last '/' in URL. Note that '/' was converted
// to ' ' in Safename
func safenameToFilename(safename string) string {
	comp := strings.Split(safename, " ")
	last := comp[len(comp)-1]
	// Drop "."sha256 tail part of Safename
	i := strings.LastIndex(last, ".")
	if i == -1 {
		log.Fatal("Malformed safename with no .sha256",
			safename)
	}
	last = last[0:i]
	return last
}

func handleModify(statusFilename string, configArg interface{},
	statusArg interface{}) {
	var config *types.VerifyImageConfig
	var status *types.VerifyImageStatus

	switch configArg.(type) {
	default:
		log.Fatal("Can only handle VerifyImageConfig")
	case *types.VerifyImageConfig:
		config = configArg.(*types.VerifyImageConfig)
	}
	switch statusArg.(type) {
	default:
		log.Fatal("Can only handle VerifyImageStatus")
	case *types.VerifyImageStatus:
		status = statusArg.(*types.VerifyImageStatus)
	}
	log.Printf("handleModify(%v) for %s\n",
		config.Safename, config.DownloadURL)

	// Note no comparison on version

	// Always update RefCount
	status.RefCount = config.RefCount

	if status.RefCount == 0 {
		status.PendingModify = true
		writeVerifyImageStatus(status, statusFilename)
		doDelete(status)
		status.PendingModify = false
		status.State = 0 // XXX INITIAL implies failure
		writeVerifyImageStatus(status, statusFilename)
		log.Printf("handleModify done for %s\n", config.DownloadURL)
		return
	}

	// If identical we do nothing. Otherwise we do a delete and create.
	if config.Safename == status.Safename &&
		config.ImageSha256 == status.ImageSha256 {
		log.Printf("handleModify: no change for %s\n",
			config.DownloadURL)
		return
	}

	status.PendingModify = true
	writeVerifyImageStatus(status, statusFilename)
	handleDelete(statusFilename, status)
	handleCreate(statusFilename, config)
	status.PendingModify = false
	writeVerifyImageStatus(status, statusFilename)
	log.Printf("handleModify done for %s\n", config.DownloadURL)
}

func handleDelete(statusFilename string, statusArg interface{}) {
	var status *types.VerifyImageStatus

	switch statusArg.(type) {
	default:
		log.Fatal("Can only handle VerifyImageStatus")
	case *types.VerifyImageStatus:
		status = statusArg.(*types.VerifyImageStatus)
	}
	log.Printf("handleDelete(%v)\n", status.Safename)

	doDelete(status)

	// Write out what we modified to VerifyImageStatus aka delete
	if err := os.Remove(statusFilename); err != nil {
		log.Println(err)
	}
	log.Printf("handleDelete done for %s\n", status.Safename)
}

// Remove the file from any of the three directories
// Only if it verified (state DELIVERED) do we detete the final. Needed
// to avoid deleting a different verified file with same sha as this claimed
// to have
func doDelete(status *types.VerifyImageStatus) {
	log.Printf("doDelete(%v)\n", status.Safename)

	pendingDirname := imgCatalogDirname + "/pending/" + status.ImageSha256
	verifierDirname := imgCatalogDirname + "/verifier/" + status.ImageSha256
	finalDirname := imgCatalogDirname + "/verified/" + status.ImageSha256

	if _, err := os.Stat(pendingDirname); err == nil {
		log.Printf("doDelete removing %s\n", pendingDirname)
		if err := os.RemoveAll(pendingDirname); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := os.Stat(verifierDirname); err == nil {
		log.Printf("doDelete removing %s\n", verifierDirname)
		if err := os.RemoveAll(verifierDirname); err != nil {
			log.Fatal(err)
		}
	}

	if status.State == types.DELIVERED {
		log.Printf("doDelete removing %s\n", finalDirname)
		if _, err := os.Stat(finalDirname); err == nil {
			if err := os.RemoveAll(finalDirname); err != nil {
				log.Fatal(err)
			}
		}
	}
	log.Printf("doDelete(%v) done\n", status.Safename)
}