//  Copyright (c) 2017 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package middleware

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/troubling/hummingbird/common"
	"github.com/troubling/hummingbird/common/conf"
	"github.com/troubling/hummingbird/common/srv"
)

var maxManifestSize = 1024 * 1024 * 2 // TODO add a check for this
var maxManifestLen = 1000

type xloMiddleware struct {
	next http.Handler
}

type segItem struct {
	Hash         string `json:"hash"`
	LastModified string `json:"last_modified"`
	Bytes        int64  `json:"bytes"`
	Name         string `json:"name"`
	ContentType  string `json:"content_type"`
	Range        string `json:"range,omitempty"`
	SubSlo       bool   `json:"sub_slo,omitempty"`
}

func (si segItem) segLenHash() (int64, string) {
	if si.Range != "" {
		segRange := si.makeRange()
		return segRange.End - segRange.Start, fmt.Sprintf(
			"%s:%s;", si.Hash, si.Range)
	}
	return int64(si.Bytes), si.Hash
}

// will return the segment range specified, or a range for the whole body
func (si segItem) makeRange() common.HttpRange {
	if si.Range != "" {
		ranges, err := common.ParseRange(fmt.Sprintf("bytes=%s", si.Range), int64(si.Bytes))
		if err == nil && len(ranges) == 1 {
			return ranges[0]
		}
	}
	return common.HttpRange{Start: 0, End: int64(si.Bytes)}
}

type sloPutManifest struct {
	Path      string `json:"path"`
	Etag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
	Range     string `json:"range,omitempty"`
}

func splitSegPath(thePath string) (string, string, error) {
	segPathParts := strings.SplitN(strings.TrimLeft(thePath, "/"), "/", 2)
	if len(segPathParts) != 2 || segPathParts[0] == "" || segPathParts[1] == "" {
		return "", "", errors.New(fmt.Sprintf("invalid segment path: %s", thePath))
	}
	return segPathParts[0], segPathParts[1], nil
}

type segWriter struct {
	http.ResponseWriter
	Status           int
	ContentLength    int64
	ContentRange     string
	Etag             string
	ContentType      string
	LastModified     string
	isSlo            bool
	isDlo            bool
	dloHeader        string
	cacheBytes       bool
	throwAwayHeader  bool
	Error            error
	manifestBytes    *bytes.Buffer
	allowWriteHeader bool
	allowWrite       bool
	xloFuncName      string
}

func (sw *segWriter) WriteUpstreamHeader() {
	if sw.isSlo {
		sw.Header().Set("X-Static-Large-Object", "True")
	}
	sw.ResponseWriter.WriteHeader(sw.Status)
	sw.Header().Del("X-Static-Large-Object")
}

func (sw *segWriter) WriteHeader(status int) {
	sw.Status = status
	if sw.throwAwayHeader { // DLO segments cannot be sub-DLOs. just treat as bytes
		return
	}
	sw.ContentLength, _ = strconv.ParseInt(sw.Header().Get("Content-Length"), 10, 64)
	sw.ContentRange = sw.Header().Get("Content-Range")
	sw.Etag = strings.Trim(sw.Header().Get("Etag"), "\"")
	sw.ContentType = sw.Header().Get("Content-Type")
	sw.LastModified = sw.Header().Get("Last-Modified")
	sw.manifestBytes = bytes.NewBuffer(make([]byte, 0))
	if theDlo := sw.Header().Get("X-Object-Manifest"); theDlo != "" && sw.xloFuncName != "get" {
		sw.isDlo = true
		sw.dloHeader = theDlo
		sw.allowWrite = false // because DLO manifests can be segments- do not write out bytes yet
	}
	if isSlo := sw.Header().Get("X-Static-Large-Object"); isSlo == "True" {
		sw.isSlo = true
		sw.cacheBytes = true
		sw.Header().Del("X-Static-Large-Object")
	}
	if !sw.isSlo && !sw.isDlo && sw.allowWriteHeader {
		sw.ResponseWriter.WriteHeader(status)
	}
}

func (sw *segWriter) Write(b []byte) (int, error) {
	if sw.cacheBytes {
		sw.manifestBytes.Write(b)
		return len(b), nil
	}
	if sw.allowWrite {
		return sw.ResponseWriter.Write(b)
	}
	return len(b), nil
}

func needToRefetchManifest(sw *segWriter, request *http.Request) bool {
	if request.Method == "HEAD" {
		return true
	}
	//TODO: what does the if-match stuff mean? if ((req.if_match or req.if_none_match) and in swift
	if request.Header.Get("Range") != "" && (sw.Status == 416) {
		return true
	}
	if request.Header.Get("Range") != "" && (sw.Status == 200 || sw.Status == 206) {
		re := regexp.MustCompile(`bytes (\d+)-(\d+)/(\d+)$`)
		res := re.FindStringSubmatch(sw.ContentRange)
		if res == nil || len(res) != 4 {
			return true
		}
		end, _ := strconv.ParseInt(res[2], 10, 64)
		length, _ := strconv.ParseInt(res[3], 10, 64)
		got_everything := (res[1] == "0" && end == length-1)
		return !got_everything
	}
	return false
}

func (xlo *xloMiddleware) feedOutSegments(sw *segWriter, request *http.Request, manifest []segItem, reqRange common.HttpRange, count int) {
	ctx := GetProxyContext(request)
	if count > 10 {
		ctx.Logger.Error(fmt.Sprintf("max recursion depth: %s", request.URL.Path),
			zap.String("MaxDepth", "10"))
		return
	}
	pathMap, err := common.ParseProxyPath(request.URL.Path)
	if err != nil || pathMap["account"] == "" {
		ctx.Logger.Error(fmt.Sprintf("invalid origReq path: %s", request.URL.Path),
			zap.Error(err))
		return
	}
	for _, si := range manifest {
		segLen, _ := si.segLenHash()
		if reqRange.Start >= segLen {
			reqRange.Start -= segLen
			reqRange.End -= segLen
			if reqRange.End < 0 {
				return
			}
			continue
		}
		if reqRange.End < 0 {
			return
		}
		segmentRange := si.makeRange()
		subReqStart := segmentRange.Start
		if reqRange.Start > 0 {
			subReqStart += reqRange.Start
		}
		subReqEnd := segmentRange.End
		if subReqEnd > segmentRange.Start+reqRange.End {
			subReqEnd = segmentRange.Start + reqRange.End
		}
		if subReqEnd <= 0 {
			continue
		}
		container, object, err := splitSegPath(si.Name)
		if err != nil {
			return
		}
		newPath := fmt.Sprintf("/v1/%s/%s/%s", pathMap["account"], container, object)
		if !si.SubSlo {
			newReq, err := http.NewRequest("GET", newPath, http.NoBody)
			if err != nil {
				ctx.Logger.Error(fmt.Sprintf("error building subrequest: %s", err),
					zap.Error(err))
				return
			}
			rangeHeader := fmt.Sprintf("bytes=%d-%d", subReqStart, subReqEnd-1)
			newReq.Header.Set("Range", rangeHeader)
			sw := &segWriter{ResponseWriter: sw.ResponseWriter,
				Status: 500, allowWrite: true, throwAwayHeader: true} // TODO i think i can reuse this
			ctx.Subrequest(sw, newReq, "slo", false)
			if sw.Status/100 != 2 {
				ctx.Logger.Debug(fmt.Sprintf("segment not found: %s", newPath),
					zap.String("Segment404", "404"))
				break
			}
		} else {
			subManifest, err := xlo.buildSloManifest(sw, request, newPath)
			if err != nil {
				ctx.Logger.Error(fmt.Sprintf("error building submanifest: %s", err),
					zap.Error(err))
				return
			}
			subRange := common.HttpRange{Start: subReqStart, End: subReqEnd}
			xlo.feedOutSegments(sw, request, subManifest, subRange, count+1)
		}
		reqRange.Start -= segLen
		reqRange.End -= segLen
	}
}

func (xlo *xloMiddleware) buildSloManifest(sw *segWriter, request *http.Request, manPath string) (manifest []segItem, err error) {

	ctx := GetProxyContext(request)
	var manifestBytes []byte
	newReq, err := http.NewRequest("GET", fmt.Sprintf("%s?multipart-manifest=get", manPath), http.NoBody)
	if err != nil {
		return manifest, err
	}

	swRefetch := &segWriter{ResponseWriter: sw.ResponseWriter, Status: 500}
	ctx.Subrequest(swRefetch, newReq, "slo", false)
	if swRefetch.manifestBytes != nil {
		manifestBytes = swRefetch.manifestBytes.Bytes()
	}
	err = json.Unmarshal(manifestBytes, &manifest)
	return manifest, err
}

func (xlo *xloMiddleware) buildDloManifest(sw *segWriter, request *http.Request, account string, container string, prefix string) (manifest []segItem, err error) {

	ctx := GetProxyContext(request)
	var manifestBytes []byte
	newReq, err := http.NewRequest("GET", fmt.Sprintf("/v1/%s/%s?format=json&prefix=%s", account, container, prefix), http.NoBody)
	if err != nil {
		return manifest, err
	}
	swRefetch := &segWriter{ResponseWriter: sw.ResponseWriter, Status: 500, cacheBytes: true}
	ctx.Subrequest(swRefetch, newReq, "slo", false)
	if swRefetch.manifestBytes != nil {
		manifestBytes = swRefetch.manifestBytes.Bytes()
	}
	if err = json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, err
	}
	for i := range manifest {
		manifest[i].Name = fmt.Sprintf("%s/%s", container, manifest[i].Name)
	}
	return manifest, nil
}

func convertManifest(manifestBytes []byte) ([]byte, error) {
	var savedManifest []segItem
	var putManifest []sloPutManifest
	err := json.Unmarshal(manifestBytes, &savedManifest)
	if err != nil {
		return []byte{}, err
	}
	for _, si := range savedManifest {
		putManifest = append(putManifest, sloPutManifest{
			Path: si.Name, Etag: si.Hash, SizeBytes: si.Bytes, Range: si.Range})
	}
	newBody, err := json.Marshal(putManifest)
	if err != nil {
		return []byte{}, err
	}
	return []byte(newBody), nil
}

func (xlo *xloMiddleware) byteFeeder(sw *segWriter, request *http.Request, xloEtag string, xloContentLengthStr string, manifest []segItem) {
	xloContentLength := int64(0)
	if xloContentLengthStr != "" {
		if cl, err := strconv.ParseInt(xloContentLengthStr, 10, 64); err == nil {
			xloContentLength = cl
		} else {
			xloContentLengthStr = ""
		}
	}
	if xloEtag == "" || xloContentLengthStr == "" {
		xloEtagGen := md5.New()
		xloContentLengthGen := int64(0)
		for _, si := range manifest {
			segLen, segHash := si.segLenHash()
			xloContentLengthGen += segLen
			io.WriteString(xloEtagGen, segHash)
		}
		xloEtag = fmt.Sprintf("%x", xloEtagGen.Sum(nil))
		xloContentLength = xloContentLengthGen
	}
	reqRangeStr := request.Header.Get("Range")
	reqRange := common.HttpRange{Start: 0, End: xloContentLength}
	if reqRangeStr != "" {
		if ranges, err := common.ParseRange(reqRangeStr, xloContentLength); err == nil {
			xloContentLength = 0
			if len(ranges) != 1 {
				srv.SimpleErrorResponse(sw.ResponseWriter, 400, "invalid multi range")
				return
			}
			reqRange = ranges[0]
			xloContentLength += reqRange.End - reqRange.Start
		} else {
			srv.SimpleErrorResponse(sw.ResponseWriter, 400, "invalid range")
			return
		}
	}
	// TODO: think i need to copy all the object meta headers here....
	sw.Header().Set("Content-Length", strconv.FormatInt(xloContentLength, 10))
	sw.Header().Set("Content-Type", sw.ContentType)
	sw.Header().Set("Etag", fmt.Sprintf("\"%s\"", xloEtag))
	sw.Status = 200
	sw.WriteUpstreamHeader()
	// this does not validate the first segment like swift. we can add that later (never)
	xlo.feedOutSegments(sw, request, manifest, reqRange, 0)

}

func (xlo *xloMiddleware) handleDloGet(sw *segWriter, request *http.Request) {
	pathMap, err := common.ParseProxyPath(request.URL.Path)
	if err != nil || pathMap["object"] == "" {
		srv.SimpleErrorResponse(sw.ResponseWriter, 400, fmt.Sprintf(
			"invalid must multipath PUT to an object path: %s", request.URL.Path))
		return
	}
	container, prefix, err := splitSegPath(sw.dloHeader)
	if err != nil {
		srv.SimpleErrorResponse(sw.ResponseWriter, 400, "invalid dlo manifest path")
		return
	}
	manifest, err := xlo.buildDloManifest(sw, request, pathMap["account"], container, prefix)
	if err != nil {
		srv.SimpleErrorResponse(sw.ResponseWriter, 400,
			fmt.Sprintf("can not build dlo manifest at: %s?%s", container, prefix))
		return
	}
	xlo.byteFeeder(sw, request, "", "", manifest)
}

func (xlo *xloMiddleware) handleSloGet(sw *segWriter, request *http.Request) {
	// next has already been called and this is an SLO
	//TODO: what does comment at slo.py#624 mean?
	contentType, _, _ := common.ParseContentTypeForSlo(sw.Header().Get("Content-Type"), 0)
	sw.Header().Set("Content-Type", contentType)

	if sw.xloFuncName == "get" {
		manifestBytes := sw.manifestBytes.Bytes()
		var err error
		if request.URL.Query().Get("format") == "raw" {
			manifestBytes, err = convertManifest(manifestBytes)
			if err != nil {
				srv.SimpleErrorResponse(sw.ResponseWriter, 400, "invalid slo manifest")
				return
			}
		} else {
			sw.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		sw.Header().Set("Content-Length", strconv.Itoa(len(manifestBytes)))
		sw.Header().Set("Etag", strings.Trim(sw.Header().Get("Etag"), "\""))
		sw.WriteUpstreamHeader()
		sw.ResponseWriter.Write(manifestBytes)
		return
	}
	sloEtag := request.Header.Get("X-Object-Sysmeta-Slo-Etag")
	savedContentLength := request.Header.Get("X-Object-Sysmeta-Slo-Size")
	isConditional := ((request.Header.Get("If-Match") != "" ||
		request.Header.Get("If-None-Match") != "") &&
		(sw.Status == 304 || sw.Status == 412))

	if (request.Method == "HEAD" || isConditional) && (sloEtag != "" || savedContentLength != "") {

		sw.Header().Set("Content-Length", savedContentLength)
		sw.Header().Set("Etag", fmt.Sprintf("\"%s\"", sloEtag))
		sw.WriteUpstreamHeader()
		return
	}
	var manifest []segItem
	var err error
	manifestBytes := sw.manifestBytes.Bytes()
	if needToRefetchManifest(sw, request) {
		manifest, err = xlo.buildSloManifest(sw, request, request.URL.Path)
	} else {
		err = json.Unmarshal(manifestBytes, &manifest)
	}
	if err != nil {
		srv.SimpleErrorResponse(sw.ResponseWriter, 400, "invalid slo manifest")
	}
	xlo.byteFeeder(sw, request, sloEtag, savedContentLength, manifest)
}

func parsePutSloManifest(body io.ReadCloser) (manifest []sloPutManifest, errs []string) {
	dec := json.NewDecoder(body)
	if _, err := dec.Token(); err != nil {
		errs = append(errs, "Invalid manifest json- not a list.")
		return manifest, errs
	}
	i := 0
	for dec.More() {
		var manItem sloPutManifest
		if err := dec.Decode(&manItem); err == io.EOF {
			break
		} else if err != nil {
			errs = append(errs, "Invalid manifest json- invalid format.")
			break
		}
		if strings.Index(strings.TrimLeft(manItem.Path, "/"), "/") == -1 {
			errs = append(errs,
				fmt.Sprintf("Index %d: path does not refer to an object. Path must be of the form /container/object.", i))
			continue
		}
		// cant really check this here because you can send size_bytes as None now
		if manItem.SizeBytes < 0 {
			errs = append(errs,
				fmt.Sprintf("Index %d: too small; each segment must be at least 1 byte.", i))
			continue
		}
		if manItem.Range != "" {
			if strings.Count(manItem.Range, "-") != 1 {
				errs = append(errs,
					fmt.Sprintf("Index %d: invalid or multiple ranges (only one allowed)", i))
				continue
			}
		}
		manifest = append(manifest, manItem)
		if len(manifest) > maxManifestLen {
			errs = append(errs, "Invalid manifest json- too many segments")
			break
		}
		i += 1
	}
	if _, err := dec.Token(); err != nil {
		errs = append(errs, "Invalid manifest json- ending bracket.")
	}
	return manifest, errs

}

func (xlo *xloMiddleware) handleSloPut(writer http.ResponseWriter, request *http.Request) {
	pathMap, err := common.ParseProxyPath(request.URL.Path)
	if err != nil || pathMap["object"] == "" {
		srv.SimpleErrorResponse(writer, 400, fmt.Sprintf(
			"invalid must multipath PUT to an object path: %s", request.URL.Path))
		return
	}
	contentLength := request.Header.Get("Content-Length")
	if contentLength == "" && request.Header.Get("Transfer-Encoding") != "chunked" {
		srv.StandardResponse(writer, 411)
		return
	}
	if request.Header.Get("X-Copy-From") != "" {
		srv.SimpleErrorResponse(writer, 405,
			"Multipart Manifest PUTs cannot be COPY requests")
		return
	}
	manifest, errs := parsePutSloManifest(request.Body)
	if len(errs) > 0 {
		srv.SimpleErrorResponse(writer, 400, strings.Join(errs, "\n"))
		return
	}
	var toPutManifest []segItem
	i := 0
	totalSize := int64(0)
	sloEtag := md5.New()
	ctx := GetProxyContext(request)
	for _, spm := range manifest {
		spmContainer, spmObject, err := splitSegPath(spm.Path)
		if err != nil {
			errs = append(errs,
				fmt.Sprintf("invalid manifest path: %s", spm.Path))
			break
		}
		if spmContainer == pathMap["container"] && spmObject == pathMap["object"] {
			errs = append(errs,
				fmt.Sprintf("manifest cannot reference itself: %s", spm.Path))
			break
		}

		newPath := fmt.Sprintf("/v1/%s/%s/%s", pathMap["account"], spmContainer, spmObject)
		newReq, err := http.NewRequest("HEAD", newPath, http.NoBody)
		if err != nil {
			ctx.Logger.Error("Couldn't create http.Request", zap.Error(err))
			return
		}
		pw := &segWriter{ResponseWriter: writer, Status: 500}
		ctx.Subrequest(pw, newReq, "slo", false)

		if pw.Status != 200 {
			errs = append(errs,
				fmt.Sprintf("%d response on segment: %s", pw.Status, newPath))
			continue
		}
		contentLength := pw.ContentLength
		segEtag := pw.Etag
		if pw.isSlo {
			subWriter := &segWriter{ResponseWriter: writer, Status: 500}
			subManifest, err := xlo.buildSloManifest(subWriter, request, newPath)
			if err != nil {
				errs = append(errs,
					fmt.Sprintf("could not build submanifest response on segment: %s (%s)", newPath, err))
				continue
			}
			subSize := int64(0)
			subSegEtag := md5.New()
			for _, si := range subManifest {
				segLen, segHash := si.segLenHash()
				subSize += segLen
				io.WriteString(subSegEtag, segHash)
			}
			segEtag = fmt.Sprintf("%x", subSegEtag.Sum(nil))
			contentLength = subSize
		}
		if spm.SizeBytes > 0 && contentLength != spm.SizeBytes {
			errs = append(errs,
				fmt.Sprintf("Unmatching ContentLength (manifest %d) != (segment actual %d) response on segment: %s", spm.SizeBytes, contentLength, newPath))
			continue
		}
		segmentSize := contentLength
		parsedRange := spm.Range
		if spm.Range != "" {
			ranges, err := common.ParseRange(fmt.Sprintf("bytes=%s", spm.Range), contentLength)
			if err != nil {
				errs = append(errs,
					fmt.Sprintf("Index %d: invalid range", i))
				continue
			}
			if len(ranges) != 1 {
				errs = append(errs,
					fmt.Sprintf("Index %d:  multiple ranges (only one allowed)", i))
				continue
			}
			segmentSize = int64(ranges[0].End - ranges[0].Start)
			parsedRange = fmt.Sprintf("%d-%d", ranges[0].Start, ranges[0].End-1) // why -1? because...
		}
		totalSize += segmentSize
		if spm.Etag != "" && spm.Etag != segEtag {
			errs = append(errs,
				fmt.Sprintf("Etag Mismatch on %s: %s != %s", spm.Path, spm.Etag, segEtag))
			continue
		}
		lastModDate, _ := common.ParseDate(pw.LastModified)

		contentType, _, _ := common.ParseContentTypeForSlo(pw.ContentType, 0)
		newSi := segItem{Name: spm.Path, Bytes: contentLength,
			Hash: segEtag, Range: parsedRange, SubSlo: pw.isSlo,
			ContentType:  contentType,
			LastModified: lastModDate.Format("2006-01-02T15:04:05.00000")}
		_, newSiHash := newSi.segLenHash()
		io.WriteString(sloEtag, newSiHash)
		toPutManifest = append(toPutManifest, newSi)
	}
	if len(errs) > 0 {
		srv.SimpleErrorResponse(writer, 400, strings.Join(errs, "\n"))
		return
	}
	xloEtagGen := fmt.Sprintf("%x", sloEtag.Sum(nil))
	if reqEtag := request.Header.Get("Etag"); reqEtag != "" {
		if strings.Trim(reqEtag, "\"") != xloEtagGen {
			srv.SimpleErrorResponse(writer, 422, "Invalid Etag")
		}
	}
	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		pathMap, _ := common.ParseProxyPath(request.URL.Path)
		contentType = mime.TypeByExtension(filepath.Ext(pathMap["object"]))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	request.Header.Set("Content-Type", fmt.Sprintf("%s;swift_bytes=%d", contentType, totalSize))
	request.Header.Set("X-Static-Large-Object", "True")
	request.Header.Set("X-Object-Sysmeta-Slo-Etag", xloEtagGen)
	request.Header.Set("X-Object-Sysmeta-Slo-Size", fmt.Sprintf("%d", totalSize))
	newBody, err := json.Marshal(toPutManifest)
	if err != nil {
		srv.SimpleErrorResponse(writer, 400, "could not build slo manifest")
		return
	}
	request.Header.Set("Etag", fmt.Sprintf("%x", md5.Sum(newBody)))
	request.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	request.Body = ioutil.NopCloser(bytes.NewReader(newBody))

	pw := &segWriter{ResponseWriter: writer, Status: 500, isSlo: true}
	xlo.next.ServeHTTP(pw, request)
	pw.WriteUpstreamHeader()
	return
}

func (xlo *xloMiddleware) deleteAllSegments(sw *segWriter, request *http.Request, manifest []segItem, count int) error {
	if count > 10 {
		return errors.New("Max recusion depth exceeded on delete")
	}
	pathMap, err := common.ParseProxyPath(request.URL.Path)
	if err != nil || pathMap["account"] == "" {
		return errors.New(fmt.Sprintf(
			"invalid path to slo delete: %s", request.URL.Path))
	}
	ctx := GetProxyContext(request)
	for _, si := range manifest {
		container, object, err := splitSegPath(si.Name)
		if err != nil {
			return errors.New(fmt.Sprintf("invalid slo item: %s", si.Name))
		}
		newPath := fmt.Sprintf("/v1/%s/%s/%s", pathMap["account"], container, object)
		if si.SubSlo {
			subManifest, err := xlo.buildSloManifest(sw, request, newPath)
			if err != nil {
				return errors.New(fmt.Sprintf("invalid sub-slo manifest: %s", newPath))
			}
			if err = xlo.deleteAllSegments(
				sw, request, subManifest, count+1); err != nil {
				return err
			}
		}
		newReq, err := http.NewRequest("DELETE", newPath, http.NoBody)
		if err != nil {
			return errors.New(fmt.Sprintf("error building subrequest: %s", err))
		}
		sw := &segWriter{ResponseWriter: sw.ResponseWriter,
			Status: 500, allowWrite: true} // TODO i think i can reuse this
		ctx.Subrequest(sw, newReq, "slo", false)
	}
	return nil
}

func (xlo *xloMiddleware) handleSloDelete(writer http.ResponseWriter, request *http.Request) {
	pathMap, err := common.ParseProxyPath(request.URL.Path)
	if err != nil || pathMap["object"] == "" {
		srv.SimpleErrorResponse(writer, 400, fmt.Sprintf(
			"invalid must multipath DELETE to an object path: %s", request.URL.Path))
		return
	}
	sw := &segWriter{ResponseWriter: writer, Status: 500}
	manifest, err := xlo.buildSloManifest(sw, request, request.URL.Path)
	if err != nil {
		srv.SimpleErrorResponse(writer, 400, fmt.Sprintf(
			"invalid manifest json: %s", err))
		return

	}
	dw := &segWriter{ResponseWriter: writer, Status: 500, allowWriteHeader: true, allowWrite: true}
	if err = xlo.deleteAllSegments(dw, request, manifest, 0); err != nil {
		srv.SimpleErrorResponse(writer, 400, fmt.Sprintf(
			"error deleting slo: %s", err))
	}
	xlo.next.ServeHTTP(writer, request)
	return
}

func updateEtagIsAt(request *http.Request, etagLoc string) {
	curHeader := request.Header.Get("X-Backend-Etag-Is-At")
	if curHeader == "" {
		curHeader = etagLoc
	} else {
		curHeader = fmt.Sprintf("%s,%s", curHeader, etagLoc)
	}
	request.Header.Set("X-Backend-Etag-Is-At", curHeader)
}

func isValidDloHeader(manifest string) bool {
	if !strings.HasPrefix(manifest, "/") &&
		strings.Index(manifest, "?") == -1 &&
		strings.Index(manifest, "&") == -1 {
		m := strings.SplitN(manifest, "/", 2)
		if len(m) == 2 && m[0] != "" && m[1] != "" {
			return true
		}
	}
	return false
}

func (xlo *xloMiddleware) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	xloFuncName := request.URL.Query().Get("multipart-manifest")
	if request.Method == "PUT" && request.Header.Get("X-Object-Manifest") != "" {
		if !isValidDloHeader(request.Header.Get("X-Object-Manifest")) {
			srv.SimpleErrorResponse(writer, 400, fmt.Sprintf(
				"X-Object-Manifest must be in the format container/prefix"))
			return
		}
		if xloFuncName == "put" {
			srv.SimpleErrorResponse(writer, 400, fmt.Sprintf("Cannot be both SLO and DLO"))
			return
		}
	}
	if request.Method == "PUT" && xloFuncName == "put" {
		xlo.handleSloPut(writer, request)
		return
	}
	if request.Method == "DELETE" && xloFuncName == "delete" {
		xlo.handleSloDelete(writer, request)
		return
	}

	if request.Method == "GET" || request.Method == "HEAD" {
		updateEtagIsAt(request, "X-Object-Sysmeta-Slo-Etag")
	}

	sw := &segWriter{ResponseWriter: writer, Status: 500,
		xloFuncName: xloFuncName, allowWriteHeader: true, allowWrite: true}
	xlo.next.ServeHTTP(sw, request)

	if sw.isSlo && (request.Method == "GET" || request.Method == "HEAD") {
		xlo.handleSloGet(sw, request)
	}
	if sw.isDlo && (request.Method == "GET" || request.Method == "HEAD") {
		xlo.handleDloGet(sw, request)
	}
}

func NewXlo(config conf.Section) (func(http.Handler) http.Handler, error) {
	RegisterInfo("slo", map[string]interface{}{"max_manifest_segments": 1000, "max_manifest_size": 2097152, "min_segment_size": 1048576})
	RegisterInfo("dlo", map[string]interface{}{"max_segments": 10000})
	return func(next http.Handler) http.Handler {
		return &xloMiddleware{next: next}
	}, nil
}
