package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
)

func readDeviceV3JSON(w http.ResponseWriter, r *http.Request, body any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, deviceEnrollMaxBody))
	if err := dec.Decode(body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "bad json", http.StatusBadRequest)
		return false
	}
	return true
}

func writeDeviceV3Error(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errDeviceEnrolment):
		http.Error(w, errDeviceEnrolment.Error(), http.StatusConflict)
	case errors.Is(err, errDeviceV3Fields):
		http.Error(w, errDeviceV3Fields.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "could not save device comparison", http.StatusInternalServerError)
	}
}

func (a *apiServer) handleRevealRemoteDevice(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID, EnrolmentID, Nonce string }
	if !readDeviceV3JSON(w, r, &body) {
		return
	}
	v, err := a.trustedDevices.revealDeviceV3(body.ID, body.EnrolmentID, body.Nonce)
	if err != nil {
		writeDeviceV3Error(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *apiServer) handleVerifyRemoteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || privateRelayDevice(r) != id {
		http.Error(w, "verify from this device's private channel", http.StatusForbidden)
		return
	}
	if !a.trustedDevices.isApproved(id) {
		http.Error(w, "unknown approved device", http.StatusNotFound)
		return
	}
	var body struct {
		CodeVersion int
		Commit      string
	}
	if !readDeviceV3JSON(w, r, &body) {
		return
	}
	if body.CodeVersion != 3 {
		writeDeviceV3Error(w, errDeviceV3Fields)
		return
	}
	v, err := a.trustedDevices.startVerificationV3(id, body.Commit)
	if err != nil {
		writeDeviceV3Error(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *apiServer) finishDeviceVerification(w http.ResponseWriter, r *http.Request, reject bool) {
	if requestViaRelay(r) {
		http.Error(w, "device comparisons can only be confirmed at the computer itself", http.StatusForbidden)
		return
	}
	caller := a.identifyCaller(r)
	if !a.requireHuman(w, r, "confirm a device comparison") {
		return
	}
	// A recorded comparison outlives a later switch to approved devices only,
	// so it takes the same proof as managing devices.
	if ok, _ := a.requireDeviceManagementProof(w, r, caller, "confirm a device comparison"); !ok {
		return
	}
	var body struct{ EnrolmentID string }
	if !readDeviceV3JSON(w, r, &body) {
		return
	}
	v, err := a.trustedDevices.finishVerificationV3(r.PathValue("id"), body.EnrolmentID, reject)
	if err != nil {
		writeDeviceV3Error(w, err)
		return
	}
	if reject {
		log.Printf("remote-devices: verification rejected device=%s enrolment=%s", r.PathValue("id"), body.EnrolmentID)
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *apiServer) handleConfirmDeviceVerification(w http.ResponseWriter, r *http.Request) {
	a.finishDeviceVerification(w, r, false)
}

func (a *apiServer) handleRejectDeviceVerification(w http.ResponseWriter, r *http.Request) {
	a.finishDeviceVerification(w, r, true)
}
