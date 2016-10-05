// Package compress provides a middleware for compression via gzip and deflate.
package compress

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

var (
	// CompressMinLength is the lower bound for compression. Smaller files
	// won't be compressed.
	CompressMinLength = 256
	// CompressMaxBuf is the upper bound for buffered compression. Larger files
	// will be compressed on-the-fly.
	CompressMaxBuf = 16 * 1024
)

// List of used header keys and values, because typing
const (
	hdrAcceptEncoding         = "Accept-Encoding"
	hdrContentEncoding        = "Content-Encoding"
	hdrContentEncodingGzip    = "gzip"
	hdrContentEncodingDeflate = "deflate"
	hdrContentLength          = "Content-Length"
	hdrContentType            = "Content-Type"
	hdrTrailer                = "Trailer"
	hdrVary                   = "Vary"
)

/**************************************\
* Fused gzip + deflate compressor type *
\**************************************/

type writeCloseFlusher interface {
	io.WriteCloser
	Flush() error
}

type compType int

const (
	compNone = compType(iota)
	compGzip
	compDeflate
)

var (
	compStrings = [...]string{
		"none",
		hdrContentEncodingGzip,
		hdrContentEncodingDeflate,
	}
)

func (c compType) String() string {
	return compStrings[c]
}

func checkAcceptEncoding(hdr http.Header) compType {
	for _, enc := range strings.Split(hdr.Get(hdrAcceptEncoding), ",") {
		e := strings.TrimSpace(enc)
		for i, name := range compStrings {
			if name == e {
				return compType(i)
			}
		}
	}
	return compNone
}

func getCompressor(c compType, w io.Writer, level int) (writeCloseFlusher, error) {
	var comp writeCloseFlusher
	var err error
	switch c {
	case compGzip:
		comp, err = gzip.NewWriterLevel(w, level)
	case compDeflate:
		comp, err = flate.NewWriter(w, level)
	default:
		err = errors.New("Unknown compressor type")
	}
	return comp, errors.Wrap(err, "Opening compressor failed")
}

/*******\
* Utils *
\*******/

func checkHeaderHas(hdr http.Header, key string) bool {
	return hdr.Get(key) != ""
}
func getContentLength(hdr http.Header) int {
	clength, _ := strconv.Atoi(hdr.Get(hdrContentLength))
	return clength
}

// List of Mimetypes that is likely to be compressable
func isCompressableType(hdr http.Header) bool {
	mtype := hdr.Get(hdrContentType)
	if strings.HasPrefix(mtype, "text/") ||
		strings.HasPrefix(mtype, "image/svg") ||
		strings.HasPrefix(mtype, "application/javascript") ||
		strings.HasPrefix(mtype, "application/x-javascript") {
		return true
	}
	return false
}
func checkIsCompressable(code int, hdr http.Header) bool {
	return code == http.StatusOK &&
		getContentLength(hdr) >= CompressMinLength && // Don't compress too small files, too much overhead TODO: find good MinBuffer
		!checkHeaderHas(hdr, hdrTrailer) && // Don't know how to handle Trailers, does it matter?
		!checkHeaderHas(hdr, hdrContentEncoding) && // Don't compress more than once
		isCompressableType(hdr) // Check if Content is likely to be compressable
}

/************************\
* compressResponseWriter *
\************************/

type compressResponseWriter struct {
	http.ResponseWriter                   // underlying network connection
	z                   writeCloseFlusher // the compressor
	buf                 bytes.Buffer      // buffer in case of a small enough file

	// the writer everything is written to, either the ResponseWriter or compressor
	w io.Writer

	// which compressor to choose and with what level
	c     compType
	level int

	code int   // save code for when to write out buffered content
	err  error // last occurred error

	wroteHeader bool // keep track whether header was written (see http.ResponseWriter)
	isBuffered  bool // set when using buffer
}

func newCompressResponseWriter(w http.ResponseWriter, c compType, level int) *compressResponseWriter {
	return &compressResponseWriter{ResponseWriter: w,
		c:     c,
		level: level}
}

// Writing of the header needs to be delayed until Close()
// Only then we know the Content-Length
func (crw *compressResponseWriter) WriteHeader(code int) {
	if crw.wroteHeader {
		return
	}
	crw.wroteHeader = true

	crw.w = crw.ResponseWriter
	hdr := crw.Header()

	if checkIsCompressable(code, hdr) {
		if getContentLength(hdr) < CompressMaxBuf {
			crw.buf.Grow(CompressMaxBuf)
			crw.w = &crw.buf
			crw.code = code
			crw.isBuffered = true
		}
		crw.z, crw.err = getCompressor(crw.c, crw.w, crw.level)
		crw.w = crw.z

		// Update Headers
		hdr.Del(hdrContentLength) // we don't know the compressed size beforehand
		hdr.Set(hdrContentEncoding, crw.c.String())
		hdr.Set(hdrVary, hdrAcceptEncoding)
	}

	if !crw.isBuffered {
		crw.ResponseWriter.WriteHeader(code)
	}
}

func (crw *compressResponseWriter) Write(p []byte) (int, error) {
	if crw.err != nil {
		return 0, crw.err
	}

	if !crw.wroteHeader {
		crw.WriteHeader(http.StatusOK)
	}

	var n int

	n, crw.err = crw.w.Write(p)
	crw.err = errors.Wrap(crw.err, "Write in compressResponseWriter failed")

	return n, crw.err
}

func (crw *compressResponseWriter) Flush() {
	if crw.err != nil {
		return
	}
	if crw.z != nil {
		crw.err = errors.Wrap(crw.z.Flush(), "Flushing compressResponseWriter failed")
	}
	if flusher, ok := crw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (crw *compressResponseWriter) Close() error {
	if crw.err != nil {
		return crw.err
	}
	if flusher, ok := crw.ResponseWriter.(http.Flusher); ok {
		defer flusher.Flush()
	}
	if crw.z == nil {
		return nil
	}

	crw.err = errors.Wrap(crw.z.Close(), "Closing compressResponseWriter failed")
	if crw.err != nil {
		return crw.err
	}
	if crw.isBuffered {
		crw.Header().Set(hdrContentLength, strconv.Itoa(crw.buf.Len()))
		crw.ResponseWriter.WriteHeader(crw.code)
		_, crw.err = crw.buf.WriteTo(crw.ResponseWriter)
	}
	return crw.err
}

/*
New wraps a http.Handler and adds compression via gzip or deflate to the
response. The Middleware takes care to not compress twice and will only
compress known mimetypes. Small responses will be buffered completely and
the Content-Length header will be set accordingly. Large responses as well
as responses with unknown length will be compressed on the fly.

	...
	log.Fatal(http.ListenAndServe(":8080", compress.New(http.DefaultServeMux))
	...

*/
func New(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Look for gzip/deflate in Accept-Encoding
		comp := checkAcceptEncoding(r.Header)
		if comp == compNone {
			// Client doesn't want compression, so skipping compression
			h.ServeHTTP(w, r)
			return
		}

		crw := newCompressResponseWriter(w, comp, flate.BestCompression)
		defer func() {
			// clean even in case h panics
			if err := crw.Close(); err != nil {
				log.Printf("%v", err)
			}
		}()

		h.ServeHTTP(crw, r)
	})
}
