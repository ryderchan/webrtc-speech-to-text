package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"
)

func main() {
	http.HandleFunc("/session", handleConnections)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "./web/index.html") })
	http.Handle("/static/", http.StripPrefix("/static", http.FileServer(http.Dir("./web"))))
	log.Println("HTTP server started on :9000")
	err := http.ListenAndServe(":9000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	// 创建p2p连接
	pcconf := webrtc.Configuration{
		ICEServers:   []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	pc, err := webrtc.NewPeerConnection(pcconf)
	if err != nil {
		log.Printf("failed:%v \n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			log.Printf("Received audio track, id = %s, codec = %v\n", track.ID(), track.Codec())
			timer := time.NewTimer(5 * time.Second)
			go func() {
				for {
					_, _, err := track.ReadRTP()
					timer.Reset(1 * time.Second)
					if err != nil {
						timer.Stop()
						log.Printf("ReadRTP failed %v\n", err)
						return
					}
					// log.Printf("receive packet %v\n", packet)
				}
			}()
		}
	})

	pc.OnICEConnectionStateChange(func(connState webrtc.ICEConnectionState) {
		log.Printf("OnICEConnectionStateChange state: %s, current time:%s \n", connState.String(), time.Now().Format(time.RFC3339Nano))
	})
	pc.OnConnectionStateChange(func(connState webrtc.PeerConnectionState) {
		log.Printf("OnConnectionStateChange state: %s, current time:%s \n", connState.String(), time.Now().Format(time.RFC3339Nano))
	})
	// Register data channel creation handling
	var dataChannel *DataChannel
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())
		dataChannel = &DataChannel{}
		dataChannel.webrtcDataChanel = d

		// Register channel opening handling
		d.OnOpen(func() {
			fmt.Printf("Data channel '%s'-'%d' on Open\n", d.Label(), d.ID())
			dataChannel.StartDevDataLoop()
		})
		// Register text message handling
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Message from DataChannel '%s': '%s'\n", d.Label(), string(msg.Data))
		})
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			log.Println("Error marshaling ICE candidate:", err)
			return
		}
		dataChannel.SendData("candidate", string(candidateJSON))
		log.Printf("OnICECandidate %+v\n", candidate)
	})

	// pc的某些状态变化回调需要协商，可能给pc添加了一个轨道或者添加了一个datachannel导致的
	pc.OnNegotiationNeeded(func() {
		log.Printf("OnNegotiationNeeded\n")
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			log.Printf("error create offer %v\n", err)
		}
		err = pc.SetLocalDescription(offer)
		if err != nil {
			log.Printf("error set local descriptor%v\n", err)
		}
		dataChannel.SendData("offer", offer.SDP)
	})

	// pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	// 	log.Printf("OnTrack %v\n", receiver.Track().Codec())
	// 	playFromDisk("output.ogg", pc)
	// 	for {
	// 		// rtp, _, err := track.ReadRTP()
	// 		_, _, err := track.ReadRTP()
	// 		if err != nil {
	// 			log.Println("Error reading RTP packet:", err)
	// 			return
	// 		}
	// 		// fmt.Printf("Received audio RTP packet: %v\n", rtp)
	// 	}
	// })

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dec := json.NewDecoder(r.Body)
	req := newSessionRequest{}

	if err := dec.Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	err = pc.SetRemoteDescription(webrtc.SessionDescription{
		SDP:  req.Offer,
		Type: webrtc.SDPTypeOffer,
	})
	if err != nil {
		log.Printf("SetRemoteDescription failed:%v \n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("CreateAnswer failed:%v \n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	err = pc.SetLocalDescription(answer)
	if err != nil {
		log.Printf("SetLocalDescription failed:%v \n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(newSessionResponse{
		Answer: answer.SDP,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
	w.Write(payload)
}

func playFromDisk(audioFileName string, pc *webrtc.PeerConnection) {
	_, err := os.Stat(audioFileName)
	haveAudioFile := !os.IsNotExist(err)
	if !haveAudioFile {
		log.Printf("No audio file!\n")
		return
	}
	if haveAudioFile {
		audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
		if audioTrackErr != nil {
			panic(audioTrackErr)
		}

		rtpSender, audioTrackErr := pc.AddTrack(audioTrack)
		if audioTrackErr != nil {
			panic(audioTrackErr)
		}

		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		go func() {
			file, oggErr := os.Open(audioFileName)
			if oggErr != nil {
				panic(oggErr)
			}

			ogg, _, oggErr := oggreader.NewWith(file)
			if oggErr != nil {
				panic(oggErr)
			}

			var lastGranule uint64

			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()

			sequenceNumber := uint16(0)
			for ; true; <-ticker.C {
				pageData, pageHeader, oggErr := ogg.ParseNextPage()
				if errors.Is(oggErr, io.EOF) {
					fmt.Printf("All audio pages parsed and sent")
					break
				}

				if oggErr != nil {
					panic(oggErr)
				}

				sampleCount := float64(pageHeader.GranulePosition - lastGranule)
				lastGranule = pageHeader.GranulePosition
				sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

				packet := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    111, // Opus payload type
						SequenceNumber: sequenceNumber,
						Timestamp:      uint32(lastGranule),
						SSRC:           12345,
					},
					Payload: pageData,
				}
				sequenceNumber++

				if oggErr = audioTrack.WriteRTP(packet); oggErr != nil {
					panic(oggErr)
				}
				log.Printf("success WriteRTP %v\n", sampleDuration)
			}
		}()
	}
}

// ########################### http接口 ###########################
type newSessionRequest struct {
	Offer string `json:"offer"`
}
type newSessionResponse struct {
	Answer string `json:"answer"`
}

// ########################### 对WebRtcDataChannel封装一层 ###########################
// DataChannelMessage中Data []byte的格式定义举例 (都是json形式)
// {"key":"offer", "value":"offer的string值"}
// {"key":"answer", "value":"answer的string值"}
// {"key":"candidate", "value":"{"sdpMid":"abc", "candidate":"def", "sdpMLineIndex":"123"}"}
// {"key":"dev", "value":"随意的一个string，用于测试"}

type DataChannel struct {
	webrtcDataChanel *webrtc.DataChannel
}

func (c *DataChannel) SendData(key, value string) error { // key: offer, answer, candidate, dev
	if c == nil {
		return fmt.Errorf("DataChannel is nil")
	}
	m := make(map[string]string)
	m["key"] = key
	m["value"] = value
	v, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if sendErr := c.webrtcDataChanel.SendText(string(v)); sendErr != nil {
		return sendErr
	}
	return nil
}

func (c *DataChannel) parseData(message webrtc.DataChannelMessage) (string, string) { // key: offer, answer, candidate, dev
	var msg map[string]string
	err := json.Unmarshal(message.Data, &msg)
	if err != nil {
		log.Println(err)
		return "", ""
	}
	return msg["key"], msg["value"]
}

func (c *DataChannel) StartDevDataLoop() {
	if c == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		i := 0
		for range ticker.C {
			if sendErr := c.SendData("dev", strconv.Itoa(i)); sendErr != nil {
				fmt.Printf("SendData get error :%v\n", sendErr)
			} else {
				fmt.Printf("SendData :%v, DataChannelId:%v\n", i, c.webrtcDataChanel.ID())
			}
			i++
		}
	}()
}
