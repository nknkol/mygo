//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/png"
	"slices"
	"strings"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

type clipboardSource struct {
	data         transfer.Data
	concealed    bool
	released     func()
	pushedToHost bool
}

type clipWireRep struct {
	format string
	data   []byte
}

type clipReadResult struct {
	err  string
	reps []clipWireRep
}

type clipWriteWire struct {
	clear     bool
	concealed bool
	text      string
	html      string
	uriList   string
	png       []byte
}

func (b *Backend) Clipboard() platform.Clipboard { return clipboardBackend{b: b} }

type clipboardBackend struct{ b *Backend }

func (c clipboardBackend) WriteData(data transfer.Data, concealed bool, released func()) error {
	b := c.b
	if b == nil {
		return nil
	}
	if len(data.Items()) == 0 {
		old := b.replaceOwnedClipboard(nil)
		if old != nil && old.released != nil {
			old.released()
		}
		if released != nil {
			released()
		}
		b.send(kindClipWrite, encodeClipWriteMessage(clipWriteWire{clear: true}))
		return nil
	}
	src := &clipboardSource{
		data:      data,
		concealed: concealed,
		released:  released,
	}
	old := b.replaceOwnedClipboard(src)
	if old != nil && old.released != nil {
		old.released()
	}
	return nil
}

func (c clipboardBackend) Flush() error {
	b := c.b
	if b == nil {
		return nil
	}
	b.mu.Lock()
	src := b.clipOwned
	b.mu.Unlock()
	if src == nil {
		return nil
	}
	fmts := src.data.Formats()
	if len(fmts) == 0 {
		return nil
	}
	mat, err := src.data.Materialize(fmts)
	if err != nil {
		return err
	}
	wire := extractClipWriteWire(mat, src.concealed)
	b.send(kindClipWrite, encodeClipWriteMessage(wire))

	b.mu.Lock()
	var rel func()
	if b.clipOwned == src {
		src.data = mat
		src.pushedToHost = true
		rel = src.released
		src.released = nil
	}
	b.mu.Unlock()
	if rel != nil {
		rel()
	}
	return nil
}

func (c clipboardBackend) Close() {
	b := c.b
	if b == nil {
		return
	}
	old := b.replaceOwnedClipboard(nil)
	if old != nil && old.released != nil {
		old.released()
	}
}

func (c clipboardBackend) Clear() {
	b := c.b
	if b == nil {
		return
	}
	old := b.replaceOwnedClipboard(nil)
	if old != nil && old.released != nil {
		old.released()
	}
	b.send(kindClipWrite, encodeClipWriteMessage(clipWriteWire{clear: true}))
}

func (c clipboardBackend) Formats() []transfer.Format {
	b := c.b
	if b == nil {
		return nil
	}
	b.mu.Lock()
	src := b.clipOwned
	extMimes := slices.Clone(b.clipExtMimes)
	b.mu.Unlock()
	if src != nil {
		return src.data.Formats()
	}
	return mapHostMimesToFormats(extMimes)
}

func (c clipboardBackend) AvailableFormats() []string {
	b := c.b
	if b == nil {
		return nil
	}
	b.mu.Lock()
	src := b.clipOwned
	extMimes := slices.Clone(b.clipExtMimes)
	b.mu.Unlock()
	if src != nil {
		fmts := src.data.Formats()
		out := make([]string, len(fmts))
		for i, f := range fmts {
			out[i] = string(f)
		}
		return out
	}
	return extMimes
}

func (c clipboardBackend) ReadData(formats []transfer.Format) (transfer.Data, error) {
	b := c.b
	if b == nil {
		if len(formats) > 0 {
			return transfer.Data{}, transfer.ErrFormat
		}
		return transfer.Data{}, nil
	}
	b.mu.Lock()
	src := b.clipOwned
	startGen := b.clipGen
	b.mu.Unlock()

	if src != nil {
		want := formats
		if len(want) == 0 {
			want = src.data.Formats()
			if len(want) == 0 {
				return transfer.Data{}, nil
			}
		}
		return src.data.Materialize(want)
	}

	if b.conn == nil {
		if len(formats) > 0 {
			return transfer.Data{}, transfer.ErrFormat
		}
		return transfer.Data{}, nil
	}

	b.mu.Lock()
	b.nextClipReq++
	reqID := b.nextClipReq
	if b.clipResults == nil {
		b.clipResults = make(map[uint32]clipReadResult)
	}
	b.mu.Unlock()

	b.send(kindClipRead, encodeClipReadMessage(reqID, formats))

	var res clipReadResult
	for {
		b.mu.Lock()
		r, done := b.clipResults[reqID]
		if done {
			delete(b.clipResults, reqID)
		}
		quit := b.quit
		b.mu.Unlock()
		if done {
			res = r
			break
		}
		if quit {
			if len(formats) > 0 {
				return transfer.Data{}, transfer.ErrFormat
			}
			return transfer.Data{}, nil
		}
		b.Step()
	}

	b.mu.Lock()
	changed := b.clipGen != startGen || b.clipOwned != nil
	b.mu.Unlock()
	if changed {
		return transfer.Data{}, platform.ErrClipboardChanged
	}
	if res.err != "" {
		return transfer.Data{}, errors.New(res.err)
	}
	return buildTransferDataFromHost(res.reps, formats)
}

func (c clipboardBackend) ReadText() string {
	d, err := c.ReadData([]transfer.Format{transfer.Text})
	if err != nil {
		return ""
	}
	raw, err := d.Read(transfer.Text)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (c clipboardBackend) WriteText(text string) {
	b := c.b
	if b == nil {
		return
	}
	src := &clipboardSource{
		data:         transfer.TextData(text),
		pushedToHost: true,
	}
	old := b.replaceOwnedClipboard(src)
	if old != nil && old.released != nil {
		old.released()
	}
	b.send(kindClipWrite, encodeClipWriteMessage(clipWriteWire{text: text}))
}

func (c clipboardBackend) ReadHTML() string {
	d, err := c.ReadData([]transfer.Format{transfer.HTML})
	if err != nil {
		return ""
	}
	raw, err := d.Read(transfer.HTML)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (c clipboardBackend) WriteHTML(markup string) {
	b := c.b
	if b == nil {
		return
	}
	data := transfer.New(transfer.NewItem(
		transfer.Bytes(transfer.HTML, []byte(markup)),
		transfer.Bytes(transfer.Text, []byte(markup)),
	))
	src := &clipboardSource{
		data:         data,
		pushedToHost: true,
	}
	old := b.replaceOwnedClipboard(src)
	if old != nil && old.released != nil {
		old.released()
	}
	b.send(kindClipWrite, encodeClipWriteMessage(clipWriteWire{text: markup, html: markup}))
}

func (c clipboardBackend) ReadImage() []byte {
	d, err := c.ReadData([]transfer.Format{transfer.PNG})
	if err != nil {
		return nil
	}
	raw, err := d.Read(transfer.PNG)
	if err != nil {
		return nil
	}
	return raw
}

func (c clipboardBackend) WriteImage(pngBytes []byte) error {
	if _, err := png.Decode(bytes.NewReader(pngBytes)); err != nil {
		return err
	}
	b := c.b
	if b == nil {
		return nil
	}
	cloned := bytes.Clone(pngBytes)
	src := &clipboardSource{
		data:         transfer.New(transfer.NewItem(transfer.Bytes(transfer.PNG, cloned))),
		pushedToHost: true,
	}
	old := b.replaceOwnedClipboard(src)
	if old != nil && old.released != nil {
		old.released()
	}
	b.send(kindClipWrite, encodeClipWriteMessage(clipWriteWire{png: cloned}))
	return nil
}

func (b *Backend) replaceOwnedClipboard(next *clipboardSource) *clipboardSource {
	b.mu.Lock()
	old := b.clipOwned
	b.clipOwned = next
	b.clipGen++
	b.clipExtMimes = nil
	b.mu.Unlock()
	return old
}

// syncOwnedClipboardToHost materializes system-shareable formats of an
// application-owned clipboard when the user switches away from the app, so
// other HarmonyOS apps can paste lazy Clipboard.Write contents immediately.
func (b *Backend) syncOwnedClipboardToHost() {
	if b == nil || b.conn == nil {
		return
	}
	b.mu.Lock()
	src := b.clipOwned
	b.mu.Unlock()
	if src == nil || src.pushedToHost {
		return
	}
	wire := extractClipWriteWire(src.data, src.concealed)
	b.mu.Lock()
	if b.clipOwned == src && !src.pushedToHost {
		src.pushedToHost = true
		b.mu.Unlock()
		b.send(kindClipWrite, encodeClipWriteMessage(wire))
		return
	}
	b.mu.Unlock()
}

func extractClipWriteWire(d transfer.Data, concealed bool) clipWriteWire {
	w := clipWriteWire{concealed: concealed}
	offered := d.Formats()
	if slices.Contains(offered, transfer.Text) {
		if raw, err := d.Read(transfer.Text); err == nil {
			w.text = string(raw)
		}
	}
	if slices.Contains(offered, transfer.HTML) {
		if raw, err := d.Read(transfer.HTML); err == nil {
			w.html = string(raw)
		}
	}
	if slices.Contains(offered, transfer.URIList) {
		if raw, err := d.Read(transfer.URIList); err == nil {
			w.uriList = string(raw)
		}
	} else if slices.Contains(offered, transfer.FileList) {
		if raw, err := d.Read(transfer.FileList); err == nil {
			w.uriList = string(raw)
		}
	}
	if slices.Contains(offered, transfer.PNG) {
		if raw, err := d.Read(transfer.PNG); err == nil {
			w.png = raw
		}
	}
	return w
}

func mapHostMimesToFormats(mimes []string) []transfer.Format {
	var out []transfer.Format
	add := func(f transfer.Format) {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	for _, raw := range mimes {
		m := strings.TrimSpace(raw)
		switch strings.ToLower(m) {
		case "":
			continue
		case "text/plain", "general.plain-text":
			add(transfer.Text)
		case "text/html", "general.html":
			add(transfer.HTML)
		case "text/uri-list", "text/uri", "general.file-uri", "application/x-mygo-file-list":
			add(transfer.FileList)
			add(transfer.URIList)
		case "image/png", "pixelmap", "general.pixel-map", "image/*":
			add(transfer.PNG)
		case "text/markdown", "general.markdown":
			add(transfer.Markdown)
		default:
			if !strings.ContainsAny(m, "\x00\r\n") {
				add(transfer.Format(m))
			}
		}
	}
	return out
}

func buildTransferDataFromHost(reps []clipWireRep, requested []transfer.Format) (transfer.Data, error) {
	want := func(f transfer.Format) bool {
		return len(requested) == 0 || slices.Contains(requested, f)
	}
	var itemReps []transfer.Representation
	var uriLines []string
	seen := make(map[transfer.Format]bool)

	for _, r := range reps {
		switch r.format {
		case string(transfer.URIList), string(transfer.FileList), "text/uri", "general.file-uri":
			for _, line := range strings.Split(strings.ReplaceAll(string(r.data), "\r\n", "\n"), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					uriLines = append(uriLines, line)
				}
			}
		default:
			fmts := mapHostMimesToFormats([]string{r.format})
			for _, f := range fmts {
				if want(f) && !seen[f] {
					seen[f] = true
					itemReps = append(itemReps, transfer.Bytes(f, r.data))
				}
			}
		}
	}

	var items []transfer.Item
	if len(uriLines) > 0 && (want(transfer.FileList) || want(transfer.URIList)) {
		for idx, u := range uriLines {
			var uReps []transfer.Representation
			if idx == 0 && len(itemReps) > 0 {
				uReps = append(uReps, itemReps...)
				itemReps = nil
			}
			if want(transfer.FileList) && strings.HasPrefix(strings.ToLower(u), "file://") {
				uReps = append(uReps, transfer.Bytes(transfer.FileList, []byte(u)))
			}
			if want(transfer.URIList) {
				uReps = append(uReps, transfer.Bytes(transfer.URIList, []byte(u)))
			}
			if len(uReps) > 0 {
				items = append(items, transfer.NewItem(uReps...))
			}
		}
	}
	if len(itemReps) > 0 {
		items = append(items, transfer.NewItem(itemReps...))
	}
	if len(items) == 0 {
		if len(requested) > 0 {
			return transfer.Data{}, transfer.ErrFormat
		}
		return transfer.Data{}, nil
	}
	return transfer.New(items...), nil
}

func (b *Backend) onClipChanged(payload []byte) {
	_, external, mimes, ok := decodeClipChangedMessage(payload)
	if !ok || !external {
		return
	}
	b.mu.Lock()
	old := b.clipOwned
	b.clipOwned = nil
	b.clipGen++
	b.clipExtMimes = mimes
	b.mu.Unlock()
	if old != nil && old.released != nil {
		old.released()
	}
}

func (b *Backend) onClipData(payload []byte) {
	reqID, res, ok := decodeClipDataMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	if b.clipResults == nil {
		b.clipResults = make(map[uint32]clipReadResult)
	}
	b.clipResults[reqID] = res
	b.mu.Unlock()
}

func encodeClipWriteMessage(w clipWriteWire) []byte {
	buf := make([]byte, 4)
	if w.clear {
		buf[0] = 1
	}
	if w.concealed {
		buf[1] = 1
	}
	buf = appendWireStr(buf, w.text)
	buf = appendWireStr(buf, w.html)
	buf = appendWireStr(buf, w.uriList)
	buf = appendWireBytes(buf, w.png)
	return buf
}

func decodeClipWriteMessage(p []byte) (clipWriteWire, bool) {
	if len(p) < 4 {
		return clipWriteWire{}, false
	}
	var out clipWriteWire
	out.clear = p[0] != 0
	out.concealed = p[1] != 0
	off := 4
	var ok bool
	if out.text, off, ok = readWireStr(p, off); !ok {
		return clipWriteWire{}, false
	}
	if out.html, off, ok = readWireStr(p, off); !ok {
		return clipWriteWire{}, false
	}
	if out.uriList, off, ok = readWireStr(p, off); !ok {
		return clipWriteWire{}, false
	}
	if out.png, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return clipWriteWire{}, false
	}
	return out, true
}

func encodeClipReadMessage(reqID uint32, formats []transfer.Format) []byte {
	var buf []byte
	buf = appendUint32(buf, reqID)
	buf = appendUint32(buf, uint32(len(formats)))
	for _, f := range formats {
		buf = appendWireStr(buf, string(f))
	}
	return buf
}

func decodeClipReadMessage(p []byte) (reqID uint32, formats []string, ok bool) {
	var off int
	reqID, off, ok = readUint32(p, 0)
	if !ok {
		return 0, nil, false
	}
	count, off, ok := readUint32(p, off)
	if !ok || count > 128 {
		return 0, nil, false
	}
	formats = make([]string, 0, count)
	for range count {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return 0, nil, false
		}
		formats = append(formats, s)
	}
	if off != len(p) {
		return 0, nil, false
	}
	return reqID, formats, true
}

func encodeClipChangedMessage(changeCount uint32, external bool, mimes []string) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:], changeCount)
	if external {
		buf[4] = 1
	}
	buf = appendUint32(buf, uint32(len(mimes)))
	for _, m := range mimes {
		buf = appendWireStr(buf, m)
	}
	return buf
}

func decodeClipChangedMessage(p []byte) (changeCount uint32, external bool, mimes []string, ok bool) {
	if len(p) < 8 {
		return 0, false, nil, false
	}
	changeCount = binary.LittleEndian.Uint32(p[0:])
	external = p[4] != 0
	count, off, ok := readUint32(p, 8)
	if !ok || count > 128 {
		return 0, false, nil, false
	}
	mimes = make([]string, 0, count)
	for range count {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return 0, false, nil, false
		}
		if s != "" {
			mimes = append(mimes, s)
		}
	}
	if off != len(p) {
		return 0, false, nil, false
	}
	return changeCount, external, mimes, true
}

func encodeClipDataMessage(reqID uint32, errStr string, reps []clipWireRep) []byte {
	var buf []byte
	buf = appendUint32(buf, reqID)
	buf = appendWireStr(buf, errStr)
	buf = appendUint32(buf, uint32(len(reps)))
	for _, r := range reps {
		buf = appendWireStr(buf, r.format)
		buf = appendWireBytes(buf, r.data)
	}
	return buf
}

func decodeClipDataMessage(p []byte) (uint32, clipReadResult, bool) {
	reqID, off, ok := readUint32(p, 0)
	if !ok {
		return 0, clipReadResult{}, false
	}
	var out clipReadResult
	if out.err, off, ok = readWireStr(p, off); !ok {
		return 0, clipReadResult{}, false
	}
	count, off, ok := readUint32(p, off)
	if !ok || count > 128 {
		return 0, clipReadResult{}, false
	}
	out.reps = make([]clipWireRep, 0, count)
	for range count {
		var fmtStr string
		var data []byte
		if fmtStr, off, ok = readWireStr(p, off); !ok {
			return 0, clipReadResult{}, false
		}
		if data, off, ok = readWireBytes(p, off); !ok {
			return 0, clipReadResult{}, false
		}
		out.reps = append(out.reps, clipWireRep{format: fmtStr, data: data})
	}
	if off != len(p) {
		return 0, clipReadResult{}, false
	}
	return reqID, out, true
}
