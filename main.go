package tsplugin

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/Monibuca/engine"
	"github.com/Monibuca/engine/avformat"
	"github.com/Monibuca/engine/avformat/mpegts"
	"github.com/Monibuca/engine/util"
)

var config = struct {
	BufferLength int
	Path         string
	AutoPublish  bool
}{2048, "ts", true}

func init() {
	InstallPlugin(&PluginConfig{
		Name:    "TS",
		Type:    PLUGIN_PUBLISHER,
		Version: "1.0.1",
		UI:      util.CurrentDir("dashboard", "ui", "plugin-ts.min.js"),
		Config:  &config,
		Run: func() {
			if config.AutoPublish {
				OnSubscribeHooks.AddHook(func(s *OutputStream) {
					if s.Publisher == nil {
						go new(TS).PublishDir(s.StreamPath)
					}
				})
			}
			http.HandleFunc("/ts/list", listTsDir)
			http.HandleFunc("/ts/publish", publishTsDir)
		},
	})
}

type TSDir struct {
	StreamPath string
	TsCount    int
	TotalSize  int64
}
type TS struct {
	InputStream
	*mpegts.MpegTsStream
	TSInfo
	//TsChan     chan io.Reader
	lastDts uint64
}
type TSInfo struct {
	TotalPesCount int
	IsSplitFrame  bool
	PTS           uint64
	DTS           uint64
	PesCount      int
	BufferLength  int
	RoomInfo      *RoomInfo
}

func (ts *TS) run() {
	//defer close(ts.TsChan)
	totalBuffer := cap(ts.TsPesPktChan)
	iframeHead := []byte{0x17, 0x01, 0, 0, 0}
	pframeHead := []byte{0x27, 0x01, 0, 0, 0}
	spsHead := []byte{0xE1, 0, 0}
	ppsHead := []byte{0x01, 0, 0}
	nalLength := []byte{0, 0, 0, 0}

	for {
		select {
		case <-ts.Done():
			return
		case tsPesPkt, ok := <-ts.TsPesPktChan:
			ts.BufferLength = len(ts.TsPesPktChan)
			if ok {
				ts.TotalPesCount++
				switch tsPesPkt.PesPkt.Header.StreamID & 0xF0 {
				case mpegts.STREAM_ID_AUDIO:
					data := tsPesPkt.PesPkt.Payload
					for remainLen := len(data); remainLen > 0; {
						// AACFrameLength(13)
						// xx xxxxxxxx xxx
						frameLen := (int(data[3]&3) << 11) | (int(data[4]) << 3) | (int(data[5]) >> 5)
						if frameLen > remainLen {
							break
						}
						av := avformat.NewAVPacket(avformat.FLV_TAG_TYPE_AUDIO)
						av.Payload = data[:frameLen]
						ts.PushAudio(av)
						data = data[frameLen:remainLen]
						remainLen = remainLen - frameLen
					}

				case mpegts.STREAM_ID_VIDEO:
					var err error
					av := avformat.NewAVPacket(avformat.FLV_TAG_TYPE_VIDEO)
					ts.PTS = tsPesPkt.PesPkt.Header.Pts
					ts.DTS = tsPesPkt.PesPkt.Header.Dts
					lastDts := ts.lastDts
					dts := ts.DTS
					pts := ts.PTS
					if dts == 0 {
						dts = pts
					}
					av.Timestamp = uint32(dts / 90)
					if ts.lastDts == 0 {
						ts.lastDts = dts
					}
					compostionTime := uint32((pts - dts) / 90)
					t1 := time.Now()
					duration := time.Millisecond * time.Duration((dts-ts.lastDts)/90)
					ts.lastDts = dts
					nalus0 := bytes.SplitN(tsPesPkt.PesPkt.Payload, avformat.NALU_Delimiter2, -1)
					nalus := make([][]byte, 0)
					for _, v := range nalus0 {
						if len(v) == 0 {
							continue
						}
						nalus = append(nalus, bytes.SplitN(v, avformat.NALU_Delimiter1, -1)...)
					}
					r := bytes.NewBuffer([]byte{})
					for _, v := range nalus {
						vl := len(v)
						if vl == 0 {
							continue
						}
						isFirst := v[1]&0x80 == 0x80 //第一个分片
						switch v[0] & 0x1f {
						case avformat.NALU_SPS:
							r.Write(avformat.RTMP_AVC_HEAD)
							util.BigEndian.PutUint16(spsHead[1:], uint16(vl))
							_, err = r.Write(spsHead)
						case avformat.NALU_PPS:
							util.BigEndian.PutUint16(ppsHead[1:], uint16(vl))
							_, err = r.Write(ppsHead)
							_, err = r.Write(v)
							av.VideoFrameType = 1
							av.Payload = r.Bytes()
							ts.PushVideo(av)
							av = avformat.NewAVPacket(avformat.FLV_TAG_TYPE_VIDEO)
							av.Timestamp = uint32(dts / 90)
							r = bytes.NewBuffer([]byte{})
							continue
						case avformat.NALU_IDR_Picture:
							if isFirst {
								av.VideoFrameType = 1
								util.BigEndian.PutUint24(iframeHead[2:], compostionTime)
								_, err = r.Write(iframeHead)
							}
							util.BigEndian.PutUint32(nalLength, uint32(vl))
							_, err = r.Write(nalLength)
						case avformat.NALU_Non_IDR_Picture:
							if isFirst {
								av.VideoFrameType = 2
								util.BigEndian.PutUint24(pframeHead[2:], compostionTime)
								_, err = r.Write(pframeHead)
							} else {
								ts.IsSplitFrame = true
							}
							util.BigEndian.PutUint32(nalLength, uint32(vl))
							_, err = r.Write(nalLength)
						default:
							continue
						}
						_, err = r.Write(v)
					}
					if MayBeError(err) {
						return
					}
					av.Payload = r.Bytes()
					ts.PushVideo(av)
					t2 := time.Since(t1)
					if duration != 0 && t2 < duration {
						if duration < time.Second {
							//if ts.BufferLength > 50 {
							duration = duration - t2
							//}
							if ts.BufferLength > 300 {
								duration = duration - duration*time.Duration(ts.BufferLength)/time.Duration(totalBuffer)
							}
							time.Sleep(duration)
						} else {
							time.Sleep(time.Millisecond * 20)
							log.Printf("stream:%s,duration:%d,dts:%d,lastDts:%d\n", ts.StreamPath, duration/time.Millisecond, tsPesPkt.PesPkt.Header.Dts, lastDts)
						}
					}
				}
			}
		}
	}
}
func (ts *TS) Publish(streamPath string, publisher Publisher) (result bool) {
	if result = ts.InputStream.Publish(streamPath, publisher); result {
		ts.TSInfo.RoomInfo = &ts.Room.RoomInfo
		ts.MpegTsStream = mpegts.NewMpegTsStream(config.BufferLength)
		go ts.run()
	}
	return
}
func (ts *TS) PublishDir(streamPath string) {
	dirPath := filepath.Join(config.Path, streamPath)
	files, err := ioutil.ReadDir(dirPath)
	if err != nil || len(files) == 0 {
		return
	}
	if ts.InputStream.Publish(strings.ReplaceAll(streamPath, "\\", "/"), ts) {
		ts.TSInfo.RoomInfo = &ts.Room.RoomInfo
		ts.MpegTsStream = mpegts.NewMpegTsStream(0)
		go ts.run()
		for _, file := range files {
			fullPath := filepath.Join(dirPath, file.Name())
			if filepath.Ext(fullPath) == ".ts" {
				if data, err := os.Open(fullPath); err == nil {
					ts.Feed(data)
					data.Close()
				}
			}
		}
		ts.Close()
	}
}
func publishTsDir(w http.ResponseWriter, r *http.Request) {
	streamPath := r.URL.Query().Get("streamPath")
	go new(TS).PublishDir(streamPath)
}
func readTsDir(currentDir string) []*TSDir {
	var list []*TSDir
	abDir := filepath.Join(config.Path, currentDir)
	if items, err := ioutil.ReadDir(abDir); err == nil {
		tscount := 0
		var totalSize int64
		for _, file := range items {
			if file.IsDir() {
				list = append(list, readTsDir(filepath.Join(currentDir, file.Name()))...)
			} else if filepath.Ext(filepath.Join(abDir, file.Name())) == ".ts" {
				tscount++
				totalSize = totalSize + file.Size()
			}
		}
		if tscount > 0 {
			info := TSDir{
				currentDir, tscount, totalSize,
			}
			list = append(list, &info)
		}
	}
	return list
}
func listTsDir(w http.ResponseWriter, r *http.Request) {
	var list []*TSDir = readTsDir(".")
	bytes, err := json.Marshal(list)
	if err == nil {
		w.Write(bytes)
	} else {
		w.Write([]byte("{\"code\":1}"))
	}
}
