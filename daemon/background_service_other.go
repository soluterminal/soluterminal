//go:build !linux

package main

import "net/http"

func serviceRequestLocal(r *http.Request, _ string, _ bool) bool { return isLoopback(r) }
