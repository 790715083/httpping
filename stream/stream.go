package stream

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/grafov/m3u8"
	mhttp "github.com/qiniu/httpping/http"
	"github.com/yutopp/go-flv"
	"github.com/yutopp/go-flv/tag"
)

type Prober struct {
	Url                string
	PlayerBufferTimeMs uint32
	ProbeTimeSec       uint32
	Header             map[string]string
}

type StreamInfo struct {
	StartTime time.Time

	IsConnected         bool
	ErrCode             int
	DnsTimeMs           uint32
	TcpConnectTimeMs    uint32
	TLSHandshakeTimeMs  uint32
	TtfbMs              uint32
	FirstVideoPktTimeMs uint32
	FirstAudioPktTimeMs uint32
	TotalLagTimeMs      uint32
	TotalLagCount       uint32
	VideoFps            float32
	LagRate             float32
	HttpCode            int
	RemoteAddr          string
	LocalAddr           string
}

func (info *StreamInfo) init(tcp *mhttp.TcpWrapper, resp *http.Response) {
	info.IsConnected = true
	info.DnsTimeMs = uint32(tcp.DnsTime().Milliseconds())
	info.TcpConnectTimeMs = uint32(tcp.TcpHandshake().Milliseconds())
	info.TLSHandshakeTimeMs = uint32(tcp.TlsHandshake().Milliseconds())
	info.TtfbMs = uint32(tcp.TTFB().Milliseconds())
	info.RemoteAddr = tcp.RemoteAddr().String()
	info.LocalAddr = tcp.LocalAddr().String()
	info.HttpCode = resp.StatusCode
}

type AVPacket struct {
	pktType  uint32
	pts      uint32
	keyframe bool
}

type Player struct {
	ch           chan AVPacket
	vqueue       []AVPacket
	aqueue       []AVPacket
	bufferTimeMs time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	info         *StreamInfo
}

type Client interface {
	Connect() (*StreamInfo, error)
	Read() (*AVPacket, error)
	Close()
}

type FlvClient struct {
	url      string
	header   map[string]string
	timeout  time.Duration
	response *http.Response
	decoder  *flv.Decoder
}

func (c *FlvClient) Connect() (*StreamInfo, error) {
	info := &StreamInfo{StartTime: time.Now()}
	req, err := newRequest(c.url, nil)
	if err != nil {
		return info, err
	}

	tcp := &mhttp.TcpWrapper{}
	hc := &http.Client{
		Transport: &http.Transport{DialContext: tcp.Dial, DialTLSContext: tcp.DialTLS},
		Timeout:   c.timeout,
	}

	c.response, err = hc.Do(req)
	if err != nil {
		info.ErrCode = ErrTcpConnectTimeout
		return info, err
	}

	info.init(tcp, c.response)
	if c.response.StatusCode != 200 {
		info.ErrCode = ErrInvalidHttpCode
		return info, nil
	}

	c.decoder, err = flv.NewDecoder(c.response.Body)
	if err != nil {
		return info, err
	}

	return info, nil
}

func (c *FlvClient) Read() (*AVPacket, error) {
	flvTag := tag.FlvTag{}
	defer flvTag.Close()

	err := c.decoder.Decode(&flvTag)
	if err != nil {
		log.Println("invalid tag:", err)
		return nil, ErrTryAgain
	}

	if flvTag.TagType == tag.TagTypeVideo {
		videoData := (flvTag.Data).(*tag.VideoData)
		pts := int32(flvTag.Timestamp) + videoData.CompositionTime
		keyframe := videoData.FrameType == tag.FrameTypeKeyFrame

		return &AVPacket{
			pts:      uint32(pts),
			pktType:  PktVideo,
			keyframe: keyframe,
		}, nil
	} else if flvTag.TagType == tag.TagTypeAudio {
		pts := flvTag.Timestamp
		audioData := (flvTag.Data).(*tag.AudioData)
		if audioData.AACPacketType == tag.AACPacketTypeRaw {
			return &AVPacket{
				pts:      pts,
				pktType:  PktAudio,
				keyframe: false,
			}, nil
		}
	}

	return nil, ErrTryAgain
}

func (c *FlvClient) Close() {
	if c.response != nil {
		c.response.Body.Close()
	}
}

type TsSegment struct {
	url   string
	seqId uint64
}

type HlsClient struct {
	url           string
	secondM3u8Url string
	scheme        string
	host          string
	header        map[string]string
	timeout       time.Duration
	m3u8Ctx       context.Context
	m3u8Cancel    context.CancelFunc
	playlist      []TsSegment
	lastSeqId     int64
	mutex         sync.Mutex
	buffer        []byte
	pat           PAT
	pmt           PMT
}

func (c *HlsClient) Connect() (*StreamInfo, error) {
	info := &StreamInfo{StartTime: time.Now()}
	req, err := newRequest(c.url, nil)
	if err != nil {
		return info, err
	}

	tcp := &mhttp.TcpWrapper{}
	hc := &http.Client{
		Transport: &http.Transport{DialContext: tcp.Dial, DialTLSContext: tcp.DialTLS},
		Timeout:   c.timeout,
	}

	resp, err := hc.Do(req)
	if err != nil {
		info.ErrCode = ErrTcpConnectTimeout
		return info, err
	}
	defer resp.Body.Close()

	info.init(tcp, resp)
	if resp.StatusCode != 200 {
		info.ErrCode = ErrInvalidHttpCode
		return info, nil
	}

	c.m3u8Ctx, c.m3u8Cancel = context.WithCancel(context.Background())
	go c.downloadM3u8()

	return info, err
}

func (c *HlsClient) Read() (*AVPacket, error) {
	if len(c.buffer) == 0 {
		var url string
		c.mutex.Lock()
		if len(c.playlist) != 0 {
			url = c.playlist[0].url
			c.playlist = c.playlist[1:]
		}
		c.mutex.Unlock()

		if url == "" {
			time.Sleep(time.Second)
			return nil, ErrTryAgain
		}

		req, err := newRequest(url, nil)
		if err != nil {
			return nil, err
		}

		hc := &http.Client{Timeout: c.timeout}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			//TODO: 统计错误状态码
			return nil, ErrTryAgain
		}

		c.buffer, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	}

	return c.demux()
}

func (c *HlsClient) Close() {
	if c.m3u8Cancel != nil {
		c.m3u8Cancel()
	}
	return
}

type PAT struct {
	programs []PATProgram
}

type PATProgram struct {
	programNumber uint32
	programMapPid uint32
}

type PMT struct {
	pmtStreams []PMTStream
}

type PMTStream struct {
	elementaryPid uint32
	streamType    uint32
}

func (c *HlsClient) demux() (*AVPacket, error) {
	for len(c.buffer) >= 188 {
		data := c.buffer[:188]
		c.buffer = c.buffer[188:]

		if data[0] != 0x47 {
			return nil, ErrInvaildTsPacket
		}

		/*
		 * sync_byte                        8 bit
		 * transport_error_indicator        1 bit
		 * payload_unit_start_indicator     1 bit
		 * transport_priority               1 bit
		 * pid                              13 bit
		 * transport_scrambling_control     2 bit
		 * adaptation_field_control         2 bit
		 * continuity_count                 4 bit
		 */

		payloadUnitStartIndicator := (data[1] & 0x40) >> 6
		pid := (uint32((data[1] & 0x1f)) << 8) | uint32(data[2])
		adaptationFieldControl := (data[3] & 0x30) >> 4
		//continuityCount := (data[3] & 0x0f)

		if pid == 0x01 || pid == 0x02 || pid == 0x03 || pid == 0x11 || pid == 0x42 || pid == 0x1fff {
			/* ignore */
			continue
		}

		/*
		 * adaption_field_control：
		 * 0x00:    reserved for future use by ISO/IEC
		 * 0x01:    no adaption field, only payload
		 * 0x02:    only adaption field, no payload
		 * 0x03:    both adaption field and payload
		 */

		if adaptationFieldControl == 0x00 || adaptationFieldControl == 0x02 {
			continue
		}

		data = data[4:]

		if adaptationFieldControl == 0x03 {
			c.decodeAdaptationFiled(&data)
		}

		// decode PAT
		if pid == 0x0 {
			if payloadUnitStartIndicator != 0 {
				data = data[1:]
			}

			c.decodePAT(data)
			continue
		}

		var pmt_found bool
		for _, program := range c.pat.programs {
			if program.programMapPid == pid {
				pmt_found = true
				break
			}
		}

		// decode PMT
		if pmt_found {
			if payloadUnitStartIndicator != 0 {
				data = data[1:]
			}

			c.decodePMT(data)
			continue
		}

		return c.decodeStream(data, pid, payloadUnitStartIndicator != 0)
	}

	c.buffer = nil
	return nil, ErrTryAgain
}

func (c *HlsClient) decodeStream(data []byte, pid uint32, payloadStart bool) (*AVPacket, error) {
	var found_stream bool
	var streamType uint32
	for _, s := range c.pmt.pmtStreams {
		if pid == s.elementaryPid {
			found_stream = true
			streamType = s.streamType
			break
		}
	}

	if !found_stream || !payloadStart {
		return nil, ErrTryAgain
	}

	var pktType uint32
	if streamType == STREAM_TYPE_VIDEO_H264 || streamType == STREAM_TYPE_VIDEO_HEVC {
		pktType = PktVideo
	} else {
		pktType = PktAudio
	}

	pkt, err := c.decodePES(data, pktType)
	if err != nil {
		return nil, err
	}

	return pkt, nil
}

func (c *HlsClient) decodePES(data []byte, pktType uint32) (*AVPacket, error) {
	/* packet_start_code_prefix                     24 bslbf */
	packetStartCodePrefix := (uint32(data[0]) << 16) |
		(uint32(data[1]) << 8) |
		uint32(data[2])

	if packetStartCodePrefix != 0x000001 {
		return nil, ErrInvaildPESHeader
	}

	data = data[3:]
	streamId := uint32(data[0])
	data = data[3:]

	if streamId != 188 &&
		streamId != 190 &&
		streamId != 191 &&
		streamId != 240 &&
		streamId != 241 &&
		streamId != 255 &&
		streamId != 242 &&
		streamId != 248 {

		if data[0]&0xc0 != 0x80 {
			return nil, ErrInvaildPESHeader
		}

		data = data[1:]

		/*
		 * PTS_DTS_flags                            2  bslbf
		 * ESCR_flag                                1  bslbf
		 * ES_rate_flag                             1  bslbf
		 * DSM_trick_mode_flag                      1  bslbf
		 * additional_copy_info_flag                1  bslbf
		 * PES_CRC_flag                             1  bslbf
		 * PES_extension_flag                       1  bslbf
		 */

		PTS_DTS_flags := (data[0] & 0xc0) >> 6
		//ESCR_flag := (data[0] & 0x20) >> 5
		//ES_rate_flag := (data[0] & 0x10) >> 4
		//DSM_trick_mode_flag := (data[0] & 0x08) >> 3
		//additional_copy_info_flag := (data[0] & 0x04) >> 2
		//PES_CRC_flag := (data[0] & 0x02) >> 1
		//PES_extension_flag := (data[0] & 0x01)

		/* PES_header_data_length                    8  uimsbf */
		data = data[2:]

		if PTS_DTS_flags == 2 {
			/*
			 * '0010'                                 4  bslbf
			 * PTS [32..30]                           3  bslbf
			 * marker_bit                             1  bslbf
			 * PTS [29..15]                           15 bslbf
			 * marker_bit                             1  bslbf
			 * PTS [14..0]                            15 bslbf
			 * marker_bit                             1  bslbf
			 */

			if (data[0]&0xf0)>>4 != 2 {
				return nil, ErrInvaildPESHeader
			}

			pts := (uint32((data[0]>>1)&0x07) << 30) |
				(uint32(data[1]) << 22) |
				((uint32(data[2]>>1) & 0x7f) << 15) |
				(uint32(data[3]) << 7) |
				uint32(data[4]>>1)

			pts /= 90

			return &AVPacket{
				pts:      uint32(pts),
				pktType:  pktType,
				keyframe: true,
			}, nil

		} else if PTS_DTS_flags == 3 {
			/*
			 * '0011'                               4  bslbf
			 * PTS [32..30]                         3  bslbf
			 * marker_bit                           1  bslbf
			 * PTS [29..15]                         15 bslbf
			 * marker_bit                           1  bslbf
			 * PTS [14..0]                          15 bslbf
			 * marker_bit                           1  bslbf
			 */

			if (data[0]&0xf0)>>4 != 3 {
				return nil, ErrInvaildPESHeader
			}

			pts := (uint32((data[0]>>1)&0x07) << 30) |
				(uint32(data[1]) << 22) |
				((uint32(data[2]>>1) & 0x7f) << 15) |
				(uint32(data[3]) << 7) |
				uint32(data[4]>>1)

			pts /= 90

			return &AVPacket{
				pts:      pts,
				pktType:  pktType,
				keyframe: true,
			}, nil
		}
	}

	return nil, ErrTryAgain
}

func (c *HlsClient) decodeAdaptationFiled(data *[]byte) {
	adaptationFieldLen := (*data)[0]
	*data = (*data)[1:]

	if adaptationFieldLen > 0 {
		*data = (*data)[adaptationFieldLen:]
		return
	}
}

func (c *HlsClient) decodePAT(data []byte) {
	var pat PAT
	sectionLength := int32(data[1]&0x0f)<<8 | int32(data[2])
	data = data[8:]

	for i := int32(0); i < sectionLength-9 && len(data) != 0; i += 4 {
		programNum := uint32(data[0])<<8 | uint32(data[1])
		if programNum != 0x00 {
			programMapPid := (uint32(data[2])<<8 | uint32(data[3])) & 0x1fff
			pat.programs = append(pat.programs, PATProgram{
				programNumber: programNum,
				programMapPid: programMapPid})
		}
		data = data[4:]
	}

	c.pat = pat
}

func (c *HlsClient) decodePMT(data []byte) {
	var pmt PMT
	sectionLength := int32((data[1]&0x0f)<<8) | int32(data[2])
	programInfoLength := int32((data[10]&0x0f)<<8) | int32(data[11])
	data = data[12+programInfoLength:]

	for i := int32(0); i < sectionLength-9-5 && len(data) != 0; i += 5 {
		stream := PMTStream{}
		stream.streamType = uint32(data[0])
		stream.elementaryPid = ((uint32(data[1]) << 8) | uint32(data[2])) & 0x1fff
		esInfoLength := uint32(data[3]&0x0f)<<8 | uint32(data[4])
		data = data[5+esInfoLength:]
		pmt.pmtStreams = append(pmt.pmtStreams, stream)
	}

	if len(pmt.pmtStreams) != 0 {
		c.pmt = pmt
	}
}

func (c *HlsClient) downloadM3u8() {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.m3u8Ctx.Done():
			return

		case <-ticker.C:
			url := c.url
			if c.secondM3u8Url != "" {
				url = c.secondM3u8Url
			}

			req, err := newRequest(url, nil)
			if err != nil {
				ticker.Reset(time.Second)
				break
			}

			hc := &http.Client{}
			resp, err := hc.Do(req)
			if err != nil {
				ticker.Reset(time.Second)
				break
			}
			defer resp.Body.Close()

			interval, err := c.decodeM3u8(resp.Body)
			if err != nil {
				log.Println("parse m3u8 error:", err)
				break
			}

			ticker.Reset(interval)
		}
	}
}

func (c *HlsClient) decodeM3u8(r io.Reader) (time.Duration, error) {
	playlist, mtype, err := m3u8.DecodeFrom(r, true)
	if err != nil {
		return time.Second, err
	}

	if mtype == m3u8.MASTER {
		masterPlaylist := playlist.(*m3u8.MasterPlaylist)
		c.secondM3u8Url = masterPlaylist.Variants[0].URI
		log.Println("second m3u8 file url=", c.secondM3u8Url)
		return time.Millisecond, nil
	}

	mediaPlaylist := playlist.(*m3u8.MediaPlaylist)
	if mediaPlaylist.Closed {
		return time.Second, ErrNotLiveM3u8File
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, segment := range mediaPlaylist.Segments {
		if segment == nil || int64(segment.SeqId) <= c.lastSeqId {
			break
		}

		uri := segment.URI
		if !(strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://")) {
			if strings.HasPrefix(uri, "/") {
				uri = c.scheme + "://" + c.host + uri
			} else {
				uri = c.scheme + "://" + c.host + "/" + uri
			}
			c.lastSeqId = int64(segment.SeqId)
		}

		log.Println("new ts url=", uri)
		c.playlist = append(c.playlist, TsSegment{
			url:   uri,
			seqId: segment.SeqId,
		})

	}

	//time.Duration(mediaPlaylist.TargetDuration/2) * time.Second,
	return time.Second, nil
}

const (
	PktAudio = 0
	PktVideo = 1
)

const (
	STREAM_TYPE_AUDIO_AAC  = 0x0f
	STREAM_TYPE_VIDEO_H264 = 0x1b
	STREAM_TYPE_VIDEO_HEVC = 0x24
)

var (
	ErrUnsupportedProtocol = errors.New("unsupported protocol")
	ErrInvaildTsPacket     = errors.New("invalid ts packet")
	ErrInvaildPESHeader    = errors.New("invalid pes header")
	ErrTryAgain            = errors.New("try again")
	ErrNotLiveM3u8File     = errors.New("not live m3u8 file")
)

var (
	ErrTcpConnectTimeout = 1001
	ErrInvalidHttpCode   = 1002
	ErrInternal          = 1003
)

func (p *Prober) Do() (*StreamInfo, error) {
	u, err := url.Parse(p.Url)
	if err != nil {
		return nil, err
	}

	var client Client

	switch u.Scheme {
	case "http", "https":
		ext := path.Ext(u.Path)
		if ext == ".flv" {
			client = &FlvClient{url: p.Url}
		} else if ext == ".m3u8" {
			client = &HlsClient{url: p.Url, scheme: u.Scheme, host: u.Host, lastSeqId: -1}
		} else {
			return nil, ErrUnsupportedProtocol
		}

	default:
		return nil, ErrUnsupportedProtocol
	}

	info, err := p.do(client)

	return info, err
}

func newRequest(url string, header map[string]string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if header != nil {
		for k, v := range header {
			req.Header.Set(k, v)
		}
	}

	return req, nil
}

func (p *Prober) do(client Client) (*StreamInfo, error) {
	info, err := client.Connect()
	if err != nil {
		return info, err
	}
	defer client.Close()

	player := NewPlayer(p.PlayerBufferTimeMs, info)
	go player.Do()
	defer player.Close()

	timer := time.NewTimer(time.Duration(p.ProbeTimeSec) * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return player.info, nil
		default:
		}

		pkt, err := client.Read()
		if err != nil {
			if err == ErrTryAgain {
				continue
			}

			return player.info, err
		}

		player.ch <- *pkt
	}

	return player.info, nil
}

func NewPlayer(playerBufferTimeMs uint32, info *StreamInfo) *Player {
	if playerBufferTimeMs > 30000 {
		playerBufferTimeMs = 30000
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Player{
		ctx:          ctx,
		cancel:       cancel,
		ch:           make(chan AVPacket, 256),
		vqueue:       make([]AVPacket, 0, 256),
		aqueue:       make([]AVPacket, 0, 256),
		bufferTimeMs: time.Duration(playerBufferTimeMs),
		info:         info,
	}
}

func (p *Player) Do() {
	var frameDuration time.Duration
	var audioFrameDuration time.Duration
	var lagTime time.Time
	var startTime time.Time

	startPlay := false
	hasVideo := false
	hasAudio := false
	rebuffer := false

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			if !startPlay {
				return
			}

			if rebuffer {
				p.info.TotalLagTimeMs += uint32(time.Since(lagTime).Milliseconds())
			}

			totalPlayTimeMs := float32(time.Since(startTime).Milliseconds())
			p.info.LagRate = float32(p.info.TotalLagTimeMs) / totalPlayTimeMs

			log.Println("player cycle end")
			return

		case pkt := <-p.ch:
			if pkt.pktType == PktVideo {
				log.Println("video pkt pts=", time.Duration(pkt.pts)*time.Millisecond, len(p.vqueue))
				if !hasVideo {
					hasVideo = true
					p.info.FirstVideoPktTimeMs = uint32(time.Since(p.info.StartTime).Milliseconds())
					log.Println("receive first video=", time.Since(p.info.StartTime))
				}

				p.vqueue = append(p.vqueue, pkt)

				if !startPlay && len(p.vqueue) >= 60 {
					// estimated frame rate
					lastPts := int32(p.vqueue[0].pts)
					count := 0
					totalDuration := int32(0)

					for i := 1; i < len(p.vqueue); i++ {
						pkt := p.vqueue[i]
						if int32(pkt.pts) > lastPts && int32(pkt.pts)-lastPts < 100 {
							totalDuration += int32(pkt.pts) - lastPts
							count++
						}
						lastPts = int32(pkt.pts)
					}

					fps := float32(30)
					if totalDuration != 0 {
						fps = float32(count) / float32(totalDuration) * 1000
					}

					p.info.VideoFps = fps
					frameDuration = time.Duration(1000000.0/fps) * time.Microsecond
					bufferTime := time.Duration(len(p.vqueue)) * frameDuration

					if bufferTime >= p.bufferTimeMs*time.Millisecond {
						startPlay = true
						startTime = time.Now()
						ticker.Reset(frameDuration)

						if p.bufferTimeMs != 0 {
							pktNum := uint32(p.bufferTimeMs * time.Millisecond / frameDuration)
							p.vqueue = p.vqueue[:pktNum]
						}

						log.Println("fps=", fps, ",frame duration=", frameDuration,
							"buffer time=", p.bufferTimeMs*time.Millisecond)
					}
				}
			} else {
				//log.Println("audio pkt pts=", pkt.pts, len(p.aqueue))
				if !hasAudio {
					hasAudio = true
					p.info.FirstAudioPktTimeMs = uint32(time.Since(p.info.StartTime).Milliseconds())
					log.Println("receive first audio=", time.Since(p.info.StartTime))
				}

				p.aqueue = append(p.aqueue, pkt) //TODO:: support audio-only stream
			}

		case <-ticker.C:
			if !startPlay {
				break
			}

			var queue *[]AVPacket
			var duratuon time.Duration

			if hasVideo {
				queue = &p.vqueue
				duratuon = frameDuration
				p.aqueue = p.aqueue[0:0]
			} else if hasAudio {
				queue = &p.aqueue
				duratuon = audioFrameDuration
			}

			bufferTime := time.Duration(len(*queue)) * duratuon
			if rebuffer && bufferTime >= p.bufferTimeMs*time.Millisecond {
				rebuffer = false
				p.info.TotalLagTimeMs += uint32(time.Since(lagTime).Milliseconds())
				log.Println("rebuffer cost time=", time.Since(lagTime))
			}

			if rebuffer {
				break
			}

			if len(*queue) != 0 {
				*queue = (*queue)[1:]
			} else {
				// play lag occurs
				rebuffer = true
				p.info.TotalLagCount++
				lagTime = time.Now()
			}
		}
	}
}

func (p *Player) Close() {
	log.Println("player Close")
	if p.cancel != nil {
		p.cancel()
	}

	if p.ch != nil {
		close(p.ch)
	}
}
