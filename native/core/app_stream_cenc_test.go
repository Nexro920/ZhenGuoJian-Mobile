package core

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildTestBox(boxType string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], boxType)
	copy(out[8:], payload)
	return out
}

func buildTestFullBox(boxType string, version byte, flags uint32, payload []byte) []byte {
	body := make([]byte, 4+len(payload))
	body[0] = version
	body[1] = byte(flags >> 16)
	body[2] = byte(flags >> 8)
	body[3] = byte(flags)
	copy(body[4:], payload)
	return buildTestBox(boxType, body)
}

func buildSyntheticCENCMP4(t *testing.T, key []byte, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	iv := [16]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80}
	encrypted := append([]byte(nil), plaintext...)
	cipher.NewCTR(block, iv[:]).XORKeyStream(encrypted, encrypted)

	ftyp := buildTestBox("ftyp", []byte("isom\x00\x00\x02\x00isom"))

	tkhdPayload := make([]byte, 16)
	binary.BigEndian.PutUint32(tkhdPayload[8:12], 1)
	tkhd := buildTestFullBox("tkhd", 0, 0, tkhdPayload)

	frma := buildTestBox("frma", []byte("avc1"))
	tencPayload := make([]byte, 20)
	tencPayload[2] = 1
	tencPayload[3] = 8
	tenc := buildTestFullBox("tenc", 0, 0, tencPayload)
	schi := buildTestBox("schi", tenc)
	sinf := buildTestBox("sinf", append(frma, schi...))

	encvPayload := append(make([]byte, 78), sinf...)
	encv := buildTestBox("encv", encvPayload)
	stsdCount := make([]byte, 4)
	binary.BigEndian.PutUint32(stsdCount, 1)
	stsd := buildTestFullBox("stsd", 0, 0, append(stsdCount, encv...))

	stszPayload := make([]byte, 12)
	binary.BigEndian.PutUint32(stszPayload[4:8], 1)
	binary.BigEndian.PutUint32(stszPayload[8:12], uint32(len(encrypted)))
	stsz := buildTestFullBox("stsz", 0, 0, stszPayload)

	stscPayload := make([]byte, 16)
	binary.BigEndian.PutUint32(stscPayload[0:4], 1)
	binary.BigEndian.PutUint32(stscPayload[4:8], 1)
	binary.BigEndian.PutUint32(stscPayload[8:12], 1)
	binary.BigEndian.PutUint32(stscPayload[12:16], 1)
	stsc := buildTestFullBox("stsc", 0, 0, stscPayload)

	sencPayload := make([]byte, 4+8)
	binary.BigEndian.PutUint32(sencPayload[0:4], 1)
	copy(sencPayload[4:12], iv[:8])
	senc := buildTestFullBox("senc", 0, 0, sencPayload)

	buildWithOffset := func(mdatPayloadOffset uint32) []byte {
		stcoPayload := make([]byte, 8)
		binary.BigEndian.PutUint32(stcoPayload[0:4], 1)
		binary.BigEndian.PutUint32(stcoPayload[4:8], mdatPayloadOffset)
		stco := buildTestFullBox("stco", 0, 0, stcoPayload)

		stblBody := append([]byte(nil), stsd...)
		stblBody = append(stblBody, stsz...)
		stblBody = append(stblBody, stsc...)
		stblBody = append(stblBody, stco...)
		stblBody = append(stblBody, senc...)
		stbl := buildTestBox("stbl", stblBody)
		minf := buildTestBox("minf", stbl)
		mdia := buildTestBox("mdia", minf)
		trak := buildTestBox("trak", append(tkhd, mdia...))
		moov := buildTestBox("moov", trak)
		mdat := buildTestBox("mdat", encrypted)

		out := append([]byte(nil), ftyp...)
		out = append(out, moov...)
		out = append(out, mdat...)
		return out
	}

	probe := buildWithOffset(0)
	mdatPayloadOffset := uint32(len(probe) - len(encrypted))
	return buildWithOffset(mdatPayloadOffset)
}

func TestNativeStreamServerDecryptsCENCAndRange(t *testing.T) {
	key := []byte("0123456789abcdef")
	plaintext := []byte("HELLO_CENC_DECRYPTED_PAYLOAD_1234567890")
	mp4 := buildSyntheticCENCMP4(t, key, plaintext)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "enc.mp4", time.Time{}, bytes.NewReader(mp4))
	}))
	defer upstream.Close()

	d := &Downloader{cfg: defaultConfig(), client: upstream.Client()}
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	localURL, token := stream.nativeOpen(providerMedia{
		URL:     upstream.URL + "/video.mp4",
		CENCKey: key,
	})
	defer stream.nativeRelease(token)

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 decrypted mp4, got status=%d err=%v", resp.StatusCode, err)
	}
	if !bytes.Contains(body, plaintext) {
		t.Fatalf("decrypted body missing plaintext payload")
	}
	if bytes.Contains(body, []byte("encv")) {
		t.Fatalf("encv box was not unwrapped to avc1")
	}

	offset := bytes.Index(body, plaintext)
	if offset <= 0 {
		t.Fatalf("plaintext offset invalid: %d", offset)
	}
	req, _ := http.NewRequest(http.MethodGet, localURL, nil)
	req.Header.Set("Range", "bytes=0-15")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBody) != 16 {
		t.Fatalf("range request failed: status=%d len=%d", rangeResp.StatusCode, len(rangeBody))
	}
}

func TestNativeStreamServerEncryptedHLSSegmentAndDisguisedTS(t *testing.T) {
	tsPacket := make([]byte, 188*4)
	for i := 0; i < 4; i++ {
		tsPacket[i*188] = 0x47
		tsPacket[i*188+1] = byte(i + 1)
	}
	disguised := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00}, tsPacket...)

	var srvURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:4.0,\nseg0.png\n#EXT-X-ENDLIST\n")
		case "/seg0.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(disguised)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	srvURL = upstream.URL

	d := &Downloader{cfg: defaultConfig(), client: upstream.Client()}
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	hlsKey := []byte("fedcba9876543210")
	localURL, token := stream.nativeOpen(providerMedia{
		URL:    srvURL + "/index.m3u8",
		HLSKey: hlsKey,
	})
	defer stream.nativeRelease(token)

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	playlistBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 playlist, got %d", resp.StatusCode)
	}

	var segURL string
	for _, line := range strings.Split(string(playlistBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			segURL = trimmed
			break
		}
	}
	if segURL == "" {
		t.Fatalf("rewritten playlist missing segment URL: %s", string(playlistBytes))
	}

	segResp, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	segBody, _ := io.ReadAll(segResp.Body)
	segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("encrypted HLS segment failed with status %d: %s", segResp.StatusCode, string(segBody))
	}
	if !bytes.Equal(segBody, tsPacket) {
		t.Fatalf("disguised PNG prefix was not stripped cleanly: len=%d first=%x", len(segBody), segBody[:min(8, len(segBody))])
	}
}

func TestNativeStreamServerLocalCENCFile(t *testing.T) {
	key := []byte("abcdef0123456789")
	plaintext := []byte("LOCAL_CENC_FILE_PLAINTEXT_SAMPLE")
	mp4 := buildSyntheticCENCMP4(t, key, plaintext)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "episode.mp4")
	if err := os.WriteFile(filePath, mp4, 0600); err != nil {
		t.Fatal(err)
	}

	d := &Downloader{cfg: defaultConfig(), client: http.DefaultClient}
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	localURL, token, err := stream.nativeOpenLocal(filePath, hex.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.nativeRelease(token)

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, plaintext) {
		t.Fatalf("local CENC proxy failed: status=%d containsPlain=%v", resp.StatusCode, bytes.Contains(body, plaintext))
	}
}
