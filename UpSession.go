package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

	"github.com/golang/glog"
)

type UpSession struct {
	manager *UpSessionManager
	config  *Config

	subAccount string
	poolIndex  int

	stratumSessions map[uint32]*StratumSession
	serverConn      net.Conn
	serverReader    *bufio.Reader

	stat            AuthorizeStat
	sessionID       uint32
	versionMask     uint32
	extraNonce2Size int

	serverCapsVerRol bool
	serverCapsSubRes bool

	eventChannel chan interface{}
}

func NewUpSession(manager *UpSessionManager, config *Config, subAccount string, poolIndex int) (up *UpSession) {
	up = new(UpSession)
	up.manager = manager
	up.config = config
	up.subAccount = subAccount
	up.poolIndex = poolIndex
	up.stratumSessions = make(map[uint32]*StratumSession)
	up.stat = StatDisconnected
	return
}

func (up *UpSession) connect() (err error) {
	up.stat = StatConnecting

	pool := up.config.Pools[up.poolIndex]

	url := fmt.Sprintf("%s:%d", pool.Host, pool.Port)
	up.serverConn, err = net.DialTimeout("tcp", url, UpSessionDialTimeout)
	if err != nil {
		up.stat = StatDisconnected
		return
	}

	up.serverReader = bufio.NewReader(up.serverConn)
	up.stat = StatConnected
	return
}

func (up *UpSession) writeJSONRequestToServer(jsonData *JSONRPCRequest) (int, error) {
	if up.stat == StatConnecting || up.stat == StatDisconnected {
		return 0, ErrConnectionClosed
	}

	bytes, err := jsonData.ToJSONBytesLine()
	if err != nil {
		return 0, err
	}

	return up.serverConn.Write(bytes)
}

func (up *UpSession) initSendRequest() (err error) {
	var request JSONRPCRequest

	request.ID = "sub"
	request.Method = "mining.subscribe"
	request.SetParam(UpSessionUserAgent)
	up.writeJSONRequestToServer(&request)
	if err != nil {
		return
	}

	request.ID = "conf"
	request.Method = "mining.configure"
	request.SetParam(JSONRPCArray{"version-rolling"}, JSONRPCObj{"version-rolling.mask": "ffffffff", "version-rolling.min-bit-count": 0})
	up.writeJSONRequestToServer(&request)
	if err != nil {
		return
	}

	request.ID = "caps"
	request.Method = "agent.get_capabilities"
	request.SetParam(JSONRPCArray{"verrol" /*version rolling*/})
	up.writeJSONRequestToServer(&request)
	if err != nil {
		return
	}

	request.ID = "auth"
	request.Method = "mining.authorize"
	request.SetParam(up.subAccount, "")
	up.writeJSONRequestToServer(&request)
	if err != nil {
		return
	}

	return
}

func (up *UpSession) close() {
	up.stat = StatDisconnected
	up.serverConn.Close()
}

func (up *UpSession) IP() string {
	if up.serverConn == nil {
		pool := up.config.Pools[up.poolIndex]
		return fmt.Sprintf("%s:%d", pool.Host, pool.Port)
	}
	return up.serverConn.RemoteAddr().String()
}

func (up *UpSession) initHandleResponse() {
	magicNum, err := up.serverReader.Peek(1)
	if err != nil {
		glog.Error("peek failed, server: ", up.IP(), ", error: ", err.Error())
		up.close()
		return
	}
	if magicNum[0] == ExMessageMagicNumber {
		up.initReadExMessage()
	} else {
		up.initReadLine()
	}
}

func (up *UpSession) initReadExMessage() {
	// ex-message:
	//   magic_number	uint8_t		magic number for Ex-Message, always 0x7F
	//   type/cmd		uint8_t		message type
	//   length			uint16_t	message length (include header self)
	//   message_body	uint8_t[]	message body
	header, err := up.serverReader.ReadBytes(4)
	if err != nil {
		glog.Error("read ex-message failed, server: ", up.IP(), ", error: ", err.Error())
		up.close()
		return
	}
	len := binary.LittleEndian.Uint16(header[2:])
	if len < 4 {
		glog.Warning("Broken ex-message from server: ", up.IP(), ", content: ", header)
		up.close()
		return
	}

	len -= 4 // len 包括 header 的长度 4 字节，所以减 4
	if len > 0 {
		// 初始化阶段不处理 ex-message，所以 Discard 掉
		_, err = up.serverReader.Discard(int(len))
		if err != nil {
			glog.Error("read ex-message failed, server: ", up.IP(), ", error: ", err.Error())
			up.close()
			return
		}
	}
}

func (up *UpSession) initReadLine() {
	jsonBytes, err := up.serverReader.ReadBytes('\n')
	if err != nil {
		glog.Error("read line failed, server: ", up.IP(), ", error: ", err.Error())
		up.close()
		return
	}

	rpcData, err := NewJSONRPCLine(jsonBytes)

	// ignore the json decode error
	if err != nil {
		glog.Info("JSON decode failed, server: ", up.IP(), err.Error(), string(jsonBytes))
		return
	}

	if len(rpcData.Method) > 0 {
		switch rpcData.Method {
		case "mining.set_version_mask":
			up.handleSetVersionMask(rpcData, jsonBytes)
		case "mining.set_difficulty":
			// ignore
		case "mining.notify":
			// ignore
		default:
			glog.Info("[TODO] pool request: ", rpcData)
		}
		return
	}

	switch rpcData.ID {
	case "sub":
		up.handleSubScribeResponse(rpcData, jsonBytes)
	case "conf":
		up.handleConfigureResponse(rpcData, jsonBytes)
	case "caps":
		up.handleGetCapsResponse(rpcData, jsonBytes)
	case "auth":
		up.handleAuthorizeResponse(rpcData, jsonBytes)
	default:
		glog.Info("[TODO] pool response: ", rpcData)
	}
}

func (up *UpSession) Init() {
	err := up.connect()
	if err != nil {
		glog.Error("connect failed, server: ", up.IP(), ", error: ", err.Error())
		return
	}

	err = up.initSendRequest()
	if err != nil {
		glog.Error("write JSON request failed, server: ", up.IP(), ", error: ", err.Error())
		up.close()
		return
	}

	for up.stat != StatAuthorized && up.stat != StatDisconnected && up.stat != StatConnecting {
		up.initHandleResponse()
	}
}

func (up *UpSession) handleSetVersionMask(rpcData *JSONRPCLine, jsonBytes []byte) {
	if len(rpcData.Params) > 0 {
		versionMaskHex, ok := rpcData.Params[0].(string)
		if !ok {
			glog.Error("version mask is not a string, server: ", up.IP(), ", response: ", string(jsonBytes))
			return
		}
		versionMask, err := strconv.ParseUint(versionMaskHex, 16, 32)
		if err != nil {
			glog.Error("version mask is not a hex, server: ", up.IP(), ", response: ", string(jsonBytes))
			return
		}
		up.versionMask = uint32(versionMask)
		glog.Info("version mask update, server: ", up.IP(), ", version mask: ", versionMaskHex)
	}
}

func (up *UpSession) handleSubScribeResponse(rpcData *JSONRPCLine, jsonBytes []byte) {
	result, ok := rpcData.Result.([]interface{})
	if !ok {
		glog.Error("subscribe result is not an array, server: ", up.IP(), ", response: ", string(jsonBytes))
		up.close()
		return
	}
	if len(result) < 3 {
		glog.Error("subscribe result missing items, server: ", up.IP(), ", response: ", string(jsonBytes))
		up.close()
		return
	}
	sessionIDHex, ok := result[1].(string)
	if !ok {
		glog.Error("session id is not a string, server: ", up.IP(), ", response: ", string(jsonBytes))
		up.close()
		return
	}
	sessionID, err := strconv.ParseUint(sessionIDHex, 16, 32)
	if err != nil {
		glog.Error("session id is not a hex, server: ", up.IP(), ", response: ", string(jsonBytes))
		up.close()
		return
	}
	up.sessionID = uint32(sessionID)

	extraNonce2SizeFloat, ok := result[2].(float64)
	if !ok {
		glog.Error("extra nonce 2 size is not an integer, server: ", up.IP(), ", response: ", string(jsonBytes))
		up.close()
		return
	}
	up.extraNonce2Size = int(extraNonce2SizeFloat)
	if up.extraNonce2Size < 6 {
		glog.Error("BTCAgent is not compatible with this server: ", up.IP(), ", extra nonce 2 is too short (only ", up.extraNonce2Size, " bytes), should be at least 6 bytes")
		up.close()
		return
	}
	up.stat = StatSubScribed
}

func (up *UpSession) handleConfigureResponse(rpcData *JSONRPCLine, jsonBytes []byte) {
	glog.Info("TODO: finish handleConfigureResponse, ", string(jsonBytes))
}

func (up *UpSession) handleGetCapsResponse(rpcData *JSONRPCLine, jsonBytes []byte) {
	result, ok := rpcData.Result.(map[string]interface{})
	if !ok {
		glog.Error("get server capabilities failed, result is not an object, server: ", up.IP(), ", response: ", string(jsonBytes))
	}
	caps, ok := result["capabilities"]
	if !ok {
		glog.Error("get server capabilities failed, missing field capabilities, server: ", up.IP(), ", response: ", string(jsonBytes))
	}
	capsArr, ok := caps.([]interface{})
	if !ok {
		glog.Error("get server capabilities failed, capabilities is not an array, server: ", up.IP(), ", response: ", string(jsonBytes))
	}
	for _, capability := range capsArr {
		switch capability {
		case "verrol":
			up.serverCapsVerRol = true
		case "subres":
			up.serverCapsSubRes = true
		}
	}
	if !up.serverCapsVerRol {
		glog.Warning("[WARNING] pool server ", up.IP(), " does not support ASICBoost")
	}
	if up.config.SubmitResponseFromServer && !up.serverCapsSubRes {
		glog.Warning("[WARNING] pool server does not support sendding response to BTCAgent")
	}
}

func (up *UpSession) handleAuthorizeResponse(rpcData *JSONRPCLine, jsonBytes []byte) {
	result, ok := rpcData.Result.(bool)
	if !ok || !result {
		glog.Error("authorize failed, server: ", up.IP(), ", sub-account: ", up.subAccount, ", error: ", rpcData.Error)
		up.close()
		return
	}
	glog.Info("authorize success, server: ", up.IP(), ", sub-account: ", up.subAccount)
	up.stat = StatAuthorized
}

func (up *UpSession) Run() {
	up.handleEvent()
}

func (up *UpSession) SendEvent(event interface{}) {
	up.eventChannel <- event
}

func (up *UpSession) AddStratumSession(session *StratumSession) {
	up.SendEvent(EventAddStratumSession{session})
}

func (up *UpSession) addStratumSession(e EventAddStratumSession) {
	up.stratumSessions[e.Session.sessionID] = e.Session
	e.Session.SetUpSession(up)
}

func (up *UpSession) handleEvent() {
	for {
		event := <-up.eventChannel

		switch e := event.(type) {
		case EventAddStratumSession:
			up.addStratumSession(e)
		default:
			glog.Error("Unknown event: ", e)
		}
	}
}