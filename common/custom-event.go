// Copyright 2014 Manu Martinez-Almeida.  All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package common

import (
	"io"
	"net/http"
	"strings"
)

// Server-Sent Events
// W3C Working Draft 29 October 2009
// http://www.w3.org/TR/2009/WD-eventsource-20091029/

var writeContentType = []string{"text/event-stream"}
var noCache = []string{"no-cache"}

var fieldReplacer = strings.NewReplacer(
	"\n", "\\n",
	"\r", "\\r")

var dataReplacer = strings.NewReplacer(
	"\n", "\n",
	"\r", "\\r")

// CustomEvent does not synchronize writes to the response writer. Streaming
// callers must serialize event writes at the stream level.
type CustomEvent struct {
	Event string
	Id    string
	Retry uint
	Data  any
}

func encode(writer io.Writer, event CustomEvent) error {
	return writeData(writer, event.Data)
}

func writeData(w io.Writer, data any) error {
	str := data.(string)
	_, _ = dataReplacer.WriteString(w, str)
	if strings.HasPrefix(str, "data") {
		_, _ = io.WriteString(w, "\n\n")
	}
	return nil
}

func (r CustomEvent) Render(w http.ResponseWriter) error {
	r.WriteContentType(w)
	return encode(w, r)
}

func (r CustomEvent) WriteContentType(w http.ResponseWriter) {
	header := w.Header()
	header["Content-Type"] = writeContentType

	if _, exist := header["Cache-Control"]; !exist {
		header["Cache-Control"] = noCache
	}
}
