package main

import "context"

// entry describes one regular file in the current device inventory.
type entry struct {
	Path    string
	Size    int64
	ModTime int64
}

type fingerprint struct {
	Size    int64
	ModTime int64
	SHA256  string
}

// device deliberately exposes no operation that can mutate phone storage.
type device interface {
	Serial(context.Context) (string, error)
	// Inventory reports unique validated discoveries synchronously. Reports are
	// provisional: callers must wait for a successful complete inventory.
	Inventory(context.Context, []string, func(int, string)) ([]entry, error)
	Inspect(context.Context, string) (fingerprint, error)
	Pull(context.Context, string, string) error
}

type connectedDevice struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Model  string `json:"model"`
}

type transferProgress struct {
	Phase      string `json:"phase"`
	Scanned    int    `json:"scanned"`
	Total      int    `json:"total"`
	Verified   int    `json:"verified"`
	Checked    int    `json:"checked"`
	Missing    int    `json:"missing"`
	Mismatched int    `json:"mismatched"`
	Changed    int    `json:"changed"`
	Errors     int    `json:"errors"`
	Copied     int    `json:"copied"`
	Deleted    int    `json:"deleted"`
	Current    string `json:"current"`
}

func (c config) report(progress transferProgress) {
	if c.onProgress != nil {
		c.onProgress(progress)
	}
}
