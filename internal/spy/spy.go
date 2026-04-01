// -*- mode:go;mode:go-playground -*-
// Copyright © 2026 P, Rich
// License: MIT, see LICENSE for details

// This package provides helper functions for http.Request context.

package spy

import (
	"io"
	"net/http"
)

type ReadCloser struct {
	io.ReadCloser
	BytesRead int
}

func (r *ReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.BytesRead += n
	return n, err
}

type ResponseWriter struct {
	http.ResponseWriter
	BytesWritten int
	StatusCode   int
}

func (w *ResponseWriter) Write(p []byte) (int, error) {
	if w.StatusCode == 0 {
		w.StatusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.BytesWritten += n
	return n, err
}

func (w *ResponseWriter) WriteHeader(statusCode int) {
	w.StatusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
