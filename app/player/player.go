package player

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"time"

	"github.com/lbryio/lbrytv/config"
	"github.com/lbryio/lbrytv/internal/lbrynet"
	"github.com/lbryio/lbrytv/internal/monitor"

	ljsonrpc "github.com/lbryio/lbry.go/v2/extras/jsonrpc"
	"github.com/lbryio/lbry.go/v2/stream"
	log "github.com/sirupsen/logrus"
)

const reflectorURL = "http://blobs.lbry.io/"

type reflectedStream struct {
	URI         string
	StartByte   int64
	EndByte     int64
	SdHash      string
	Size        int64
	ContentType string
	SDBlob      *stream.SDBlob
	seekOffset  int64
}

// PlayURI downloads and streams LBRY video content located at uri and delimited by rangeHeader
// (use rangeHeader := request.Header.Get("Range")).
// Streaming works like this:
// 1. Resolve stream hash through lbrynet daemon (see resolve)
// 2. Retrieve stream details (list of blob hashes and lengths, etc) by the SD hash from the reflector
// (see fetchData)
// 3. Implement io.ReadSeeker interface for http.ServeContent:
// - Seek simply implements io.Seeker
// - Read calculates boundaries and finds blobs that contain the requested stream range,
// then calls streamBlobs, which sequentially downloads and decrypts requested blobs
func PlayURI(uri string, w http.ResponseWriter, req *http.Request) error {
	rs, err := newReflectedStream(uri)
	if err != nil {
		return err
	}
	err = rs.fetchData()
	if err != nil {
		return err
	}
	rs.prepareWriter(w)
	ServeContent(w, req, "test", time.Time{}, rs)
	return err
}

func newReflectedStream(uri string) (rs *reflectedStream, err error) {
	client := ljsonrpc.NewClient(config.GetLbrynet())
	rs = &reflectedStream{URI: uri}
	err = rs.resolve(client)
	return rs, err
}

// Read implements io.ReadSeeker interface
func (s *reflectedStream) Read(p []byte) (n int, err error) {
	var startOffsetInBlob int64

	bufferLen := len(p)
	seekOffsetEnd := s.seekOffset + int64(bufferLen)
	blobNum := int(s.seekOffset / (stream.MaxBlobSize - 2))

	if blobNum == 0 {
		startOffsetInBlob = s.seekOffset - int64(blobNum*stream.MaxBlobSize)
	} else {
		startOffsetInBlob = s.seekOffset - int64(blobNum*stream.MaxBlobSize) + int64(blobNum)
	}

	n, err = s.streamBlob(blobNum, startOffsetInBlob, p)

	monitor.Logger.WithFields(log.Fields{
		"read_buffer_length": bufferLen,
		"blob_num":           blobNum,
		"current_offset":     s.seekOffset,
		"offset_in_blob":     startOffsetInBlob,
	}).Debugf("read %v bytes (%v..%v) from blob stream", n, s.seekOffset, seekOffsetEnd)

	s.seekOffset += int64(n)
	return n, err
}

// Seek implements io.ReadSeeker interface
func (s *reflectedStream) Seek(offset int64, whence int) (int64, error) {
	var newSeekOffset int64

	if whence == io.SeekEnd {
		newSeekOffset = s.Size - offset
	} else if whence == io.SeekStart {
		newSeekOffset = offset
	} else if whence == io.SeekCurrent {
		newSeekOffset = s.seekOffset + offset
	} else {
		return 0, errors.New("invalid seek whence argument")
	}

	if 0 > newSeekOffset {
		return 0, errors.New("seeking before the beginning of file")
	}

	s.seekOffset = newSeekOffset
	return newSeekOffset, nil
}

func (s *reflectedStream) URL() string {
	return reflectorURL + s.SdHash
}

func (s *reflectedStream) resolve(client *ljsonrpc.Client) error {
	if s.URI == "" {
		return errors.New("stream uri is not set")
	}

	r, err := lbrynet.Resolve(s.URI)
	if err != nil {
		return err
	}

	// TODO: Change when underlying libs are updated for 0.38
	stream := r.Value.GetStream()
	if stream.Fee != nil && stream.Fee.Amount > 0 {
		return errors.New("paid stream")
	}

	s.SdHash = hex.EncodeToString(stream.Source.SdHash)
	s.ContentType = stream.Source.MediaType
	s.Size = int64(stream.Source.Size)

	monitor.Logger.WithFields(log.Fields{
		"sd_hash":      fmt.Sprintf("%s", s.SdHash),
		"uri":          s.URI,
		"content_type": s.ContentType,
	}).Debug("resolved uri")

	return nil
}

func (s *reflectedStream) fetchData() error {
	if s.SdHash == "" {
		return errors.New("no sd hash set, call `resolve` first")
	}
	monitor.Logger.WithFields(log.Fields{
		"uri": s.URI, "url": s.URL(),
	}).Debug("requesting stream data")

	resp, err := http.Get(s.URL())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	sdb := &stream.SDBlob{}
	err = sdb.UnmarshalJSON(body)
	if err != nil {
		return err
	}

	if s.Size == 0 {
		for _, bi := range sdb.BlobInfos {
			if bi.Length == stream.MaxBlobSize {
				s.Size += int64(stream.MaxBlobSize - 1)
			} else {
				s.Size += int64(bi.Length)
			}
		}

		// last padding is unguessable
		s.Size -= 15
	}

	sort.Slice(sdb.BlobInfos, func(i, j int) bool {
		return sdb.BlobInfos[i].BlobNum < sdb.BlobInfos[j].BlobNum
	})
	s.SDBlob = sdb

	monitor.Logger.WithFields(log.Fields{
		"blobs_number": len(sdb.BlobInfos),
		"stream_size":  s.Size,
		"uri":          s.URI,
	}).Debug("got stream data")
	return nil
}

func (s *reflectedStream) prepareWriter(w http.ResponseWriter) {
	w.Header().Set("Content-Type", s.ContentType)
}

func (s *reflectedStream) streamBlob(blobNum int, startOffsetInBlob int64, dest []byte) (n int, err error) {
	bi := s.SDBlob.BlobInfos[blobNum]
	if n > 0 {
		startOffsetInBlob = 0
	}
	url := blobInfoURL(bi)

	monitor.Logger.WithFields(log.Fields{
		"url":      url,
		"stream":   s.URI,
		"blob_num": bi.BlobNum,
	}).Debug("requesting a blob")
	start := time.Now()

	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	monitor.Logger.WithFields(log.Fields{
		"stream":       s.URI,
		"blob_num":     bi.BlobNum,
		"time_elapsed": time.Since(start),
	}).Debug("done downloading a blob")

	if blobNum == 0 {
		monitor.Logger.WithFields(log.Fields{
			"stream":          s.URI,
			"first_blob_time": time.Since(start).Seconds(),
		}).Info("stream playback requested")
	}

	if resp.StatusCode == http.StatusOK {
		start := time.Now()

		encryptedBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return 0, err
		}

		decryptedBody, err := stream.DecryptBlob(stream.Blob(encryptedBody), s.SDBlob.Key, bi.IV)
		if err != nil {
			return 0, err
		}

		endOffsetInBlob := int64(len(dest)) + startOffsetInBlob
		if endOffsetInBlob > int64(len(decryptedBody)) {
			endOffsetInBlob = int64(len(decryptedBody))
		}

		thisN := copy(dest, decryptedBody[startOffsetInBlob:endOffsetInBlob])
		n += thisN

		monitor.Logger.WithFields(log.Fields{
			"stream":        s.URI,
			"blob_num":      bi.BlobNum,
			"bytes_written": n,
			"time_elapsed":  time.Since(start),
			"start_offset":  startOffsetInBlob,
			"end_offset":    endOffsetInBlob,
		}).Debug("done streaming a blob")
	} else {
		return n, fmt.Errorf("server responded with an unexpected status (%v)", resp.Status)
	}
	return n, nil
}

func blobInfoURL(bi stream.BlobInfo) string {
	return reflectorURL + hex.EncodeToString(bi.BlobHash)
}
