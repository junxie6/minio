/*
 * MinIO Cloud Storage, (C) 2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/event"
	"github.com/minio/minio/pkg/lifecycle"
	xnet "github.com/minio/minio/pkg/net"
	"github.com/minio/minio/pkg/policy"
	trace "github.com/minio/minio/pkg/trace"
)

// To abstract a node over network.
type peerRESTServer struct {
}

func getServerInfo() (*ServerInfoData, error) {
	if globalBootTime.IsZero() {
		return nil, errServerNotInitialized
	}

	objLayer := newObjectLayerFn()
	if objLayer == nil {
		return nil, errServerNotInitialized
	}
	// Server info data.
	return &ServerInfoData{
		StorageInfo: objLayer.StorageInfo(context.Background()),
		ConnStats:   globalConnStats.toServerConnStats(),
		HTTPStats:   globalHTTPStats.toServerHTTPStats(),
		Properties: ServerProperties{
			Uptime:       UTCNow().Sub(globalBootTime),
			Version:      Version,
			CommitID:     CommitID,
			DeploymentID: globalDeploymentID,
			SQSARN:       globalNotificationSys.GetARNList(),
			Region:       globalServerConfig.GetRegion(),
		},
	}, nil
}

// uptimes - used to sort uptimes in chronological order.
type uptimes []time.Duration

func (ts uptimes) Len() int {
	return len(ts)
}

func (ts uptimes) Less(i, j int) bool {
	return ts[i] < ts[j]
}

func (ts uptimes) Swap(i, j int) {
	ts[i], ts[j] = ts[j], ts[i]
}

// getPeerUptimes - returns the uptime.
func getPeerUptimes(serverInfo []ServerInfo) time.Duration {
	// In a single node Erasure or FS backend setup the uptime of
	// the setup is the uptime of the single minio server
	// instance.
	if !globalIsDistXL {
		return UTCNow().Sub(globalBootTime)
	}

	var times []time.Duration

	for _, info := range serverInfo {
		if info.Error != "" {
			continue
		}
		times = append(times, info.Data.Properties.Uptime)
	}

	// Sort uptimes in chronological order.
	sort.Sort(uptimes(times))

	// Return the latest time as the uptime.
	return times[0]
}

// NetReadPerfInfoHandler - returns network read performance information.
func (s *peerRESTServer) NetReadPerfInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	params := mux.Vars(r)

	sizeStr, found := params[peerRESTNetPerfSize]
	if !found {
		s.writeErrorResponse(w, errors.New("size is missing"))
		return
	}

	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	start := time.Now()
	n, err := io.CopyN(ioutil.Discard, r.Body, size)
	end := time.Now()

	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	if n != size {
		s.writeErrorResponse(w, fmt.Errorf("short read; expected: %v, got: %v", size, n))
		return
	}

	addr := r.Host
	if globalIsDistXL {
		addr = GetLocalPeer(globalEndpoints)
	}

	info := ServerNetReadPerfInfo{
		Addr:     addr,
		ReadPerf: end.Sub(start),
	}

	ctx := newContext(r, w, "NetReadPerfInfo")
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
	w.(http.Flusher).Flush()
}

// CollectNetPerfInfoHandler - returns network performance information collected from other peers.
func (s *peerRESTServer) CollectNetPerfInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	params := mux.Vars(r)
	sizeStr, found := params[peerRESTNetPerfSize]
	if !found {
		s.writeErrorResponse(w, errors.New("size is missing"))
		return
	}

	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	info := globalNotificationSys.NetReadPerfInfo(size)

	ctx := newContext(r, w, "CollectNetPerfInfo")
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
	w.(http.Flusher).Flush()
}

// GetLocksHandler - returns list of older lock from the server.
func (s *peerRESTServer) GetLocksHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "GetLocks")
	locks := globalLockServer.ll.DupLockMap()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(locks))

	w.(http.Flusher).Flush()

}

// DeletePolicyHandler - deletes a policy on the server.
func (s *peerRESTServer) DeletePolicyHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	policyName := vars[peerRESTPolicy]
	if policyName == "" {
		s.writeErrorResponse(w, errors.New("policyName is missing"))
		return
	}

	if err := globalIAMSys.DeletePolicy(policyName); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// LoadPolicyHandler - reloads a policy on the server.
func (s *peerRESTServer) LoadPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	policyName := vars[peerRESTPolicy]
	if policyName == "" {
		s.writeErrorResponse(w, errors.New("policyName is missing"))
		return
	}

	if err := globalIAMSys.LoadPolicy(objAPI, policyName); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// LoadPolicyMappingHandler - reloads a policy mapping on the server.
func (s *peerRESTServer) LoadPolicyMappingHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	userOrGroup := vars[peerRESTUserOrGroup]
	if userOrGroup == "" {
		s.writeErrorResponse(w, errors.New("user-or-group is missing"))
		return
	}
	_, isGroup := vars[peerRESTIsGroup]

	if err := globalIAMSys.LoadPolicyMapping(objAPI, userOrGroup, isGroup); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// DeleteUserHandler - deletes a user on the server.
func (s *peerRESTServer) DeleteUserHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars[peerRESTUser]
	if accessKey == "" {
		s.writeErrorResponse(w, errors.New("username is missing"))
		return
	}

	if err := globalIAMSys.DeleteUser(accessKey); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// LoadUserHandler - reloads a user on the server.
func (s *peerRESTServer) LoadUserHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars[peerRESTUser]
	if accessKey == "" {
		s.writeErrorResponse(w, errors.New("username is missing"))
		return
	}

	temp, err := strconv.ParseBool(vars[peerRESTUserTemp])
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	if err = globalIAMSys.LoadUser(objAPI, accessKey, temp); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// LoadUsersHandler - reloads all users and canned policies.
func (s *peerRESTServer) LoadUsersHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	err := globalIAMSys.Load()
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// LoadGroupHandler - reloads group along with members list.
func (s *peerRESTServer) LoadGroupHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}

	vars := mux.Vars(r)
	group := vars[peerRESTGroup]
	err := globalIAMSys.LoadGroup(objAPI, group)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// StartProfilingHandler - Issues the start profiling command.
func (s *peerRESTServer) StartProfilingHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	profiler := vars[peerRESTProfiler]
	if profiler == "" {
		s.writeErrorResponse(w, errors.New("profiler name is missing"))
		return
	}

	if globalProfiler != nil {
		globalProfiler.Stop()
	}

	var err error
	globalProfiler, err = startProfiler(profiler, "")
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.(http.Flusher).Flush()
}

// ServerInfoHandler - returns server info.
func (s *peerRESTServer) ServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "ServerInfo")
	info, err := getServerInfo()
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
}

// DownloadProflingDataHandler - returns proflied data.
func (s *peerRESTServer) DownloadProflingDataHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "DownloadProfiling")
	profileData, err := getProfileData()
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(profileData))
}

// CPULoadInfoHandler - returns CPU Load info.
func (s *peerRESTServer) CPULoadInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "CPULoadInfo")
	info := localEndpointsCPULoad(globalEndpoints, r)

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
}

// DrivePerfInfoHandler - returns Drive Performance info.
func (s *peerRESTServer) DrivePerfInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "DrivePerfInfo")
	info := localEndpointsDrivePerf(globalEndpoints, r)

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
}

// MemUsageInfoHandler - returns Memory Usage info.
func (s *peerRESTServer) MemUsageInfoHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}
	ctx := newContext(r, w, "MemUsageInfo")
	info := localEndpointsMemUsage(globalEndpoints, r)

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(info))
}

// DeleteBucketHandler - Delete notification and policies related to the bucket.
func (s *peerRESTServer) DeleteBucketHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}

	globalNotificationSys.RemoveNotification(bucketName)
	globalPolicySys.Remove(bucketName)

	w.(http.Flusher).Flush()
}

// ReloadFormatHandler - Reload Format.
func (s *peerRESTServer) ReloadFormatHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	dryRunString := vars[peerRESTDryRun]
	if dryRunString == "" {
		s.writeErrorResponse(w, errors.New("dry run parameter is missing"))
		return
	}

	var dryRun bool
	switch strings.ToLower(dryRunString) {
	case "true":
		dryRun = true
	case "false":
		dryRun = false
	default:
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	objAPI := newObjectLayerFn()
	if objAPI == nil {
		s.writeErrorResponse(w, errServerNotInitialized)
		return
	}
	err := objAPI.ReloadFormat(context.Background(), dryRun)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	w.(http.Flusher).Flush()
}

// RemoveBucketPolicyHandler - Remove bucket policy.
func (s *peerRESTServer) RemoveBucketPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}

	globalPolicySys.Remove(bucketName)
	w.(http.Flusher).Flush()
}

// SetBucketPolicyHandler - Set bucket policy.
func (s *peerRESTServer) SetBucketPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}
	var policyData policy.Policy
	if r.ContentLength < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&policyData)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	globalPolicySys.Set(bucketName, policyData)
	w.(http.Flusher).Flush()
}

// RemoveBucketLifecycleHandler - Remove bucket lifecycle.
func (s *peerRESTServer) RemoveBucketLifecycleHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}

	globalLifecycleSys.Remove(bucketName)
	w.(http.Flusher).Flush()
}

// SetBucketLifecycleHandler - Set bucket lifecycle.
func (s *peerRESTServer) SetBucketLifecycleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}
	var lifecycleData lifecycle.Lifecycle
	if r.ContentLength < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&lifecycleData)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	globalLifecycleSys.Set(bucketName, lifecycleData)
	w.(http.Flusher).Flush()
}

type remoteTargetExistsResp struct {
	Exists bool
}

// TargetExistsHandler - Check if Target exists.
func (s *peerRESTServer) TargetExistsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "TargetExists")
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}
	var targetID event.TargetID
	if r.ContentLength <= 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&targetID)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	var targetExists remoteTargetExistsResp
	targetExists.Exists = globalNotificationSys.RemoteTargetExist(bucketName, targetID)

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(&targetExists))
}

type sendEventRequest struct {
	Event    event.Event
	TargetID event.TargetID
}

type sendEventResp struct {
	Success bool
}

// SendEventHandler - Send Event.
func (s *peerRESTServer) SendEventHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	ctx := newContext(r, w, "SendEvent")

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}
	var eventReq sendEventRequest
	if r.ContentLength <= 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&eventReq)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	var eventResp sendEventResp
	eventResp.Success = true
	errs := globalNotificationSys.send(bucketName, eventReq.Event, eventReq.TargetID)

	for i := range errs {
		reqInfo := (&logger.ReqInfo{}).AppendTags("Event", eventReq.Event.EventName.String())
		reqInfo.AppendTags("targetName", eventReq.TargetID.Name)
		ctx := logger.SetReqInfo(context.Background(), reqInfo)
		logger.LogIf(ctx, errs[i].Err)

		eventResp.Success = false
		s.writeErrorResponse(w, errs[i].Err)
		return
	}
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(&eventResp))
	w.(http.Flusher).Flush()
}

// PutBucketNotificationHandler - Set bucket policy.
func (s *peerRESTServer) PutBucketNotificationHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}

	var rulesMap event.RulesMap
	if r.ContentLength < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&rulesMap)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	globalNotificationSys.AddRulesMap(bucketName, rulesMap)
	w.(http.Flusher).Flush()
}

type listenBucketNotificationReq struct {
	EventNames []event.Name   `json:"eventNames"`
	Pattern    string         `json:"pattern"`
	TargetID   event.TargetID `json:"targetId"`
	Addr       xnet.Host      `json:"addr"`
}

// ListenBucketNotificationHandler - Listen bucket notification handler.
func (s *peerRESTServer) ListenBucketNotificationHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	bucketName := vars[peerRESTBucket]
	if bucketName == "" {
		s.writeErrorResponse(w, errors.New("Bucket name is missing"))
		return
	}

	var args listenBucketNotificationReq
	if r.ContentLength <= 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	err := gob.NewDecoder(r.Body).Decode(&args)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	restClient, err := newPeerRESTClient(&args.Addr)
	if err != nil {
		s.writeErrorResponse(w, fmt.Errorf("unable to find PeerRESTClient for provided address %v. This happens only if remote and this minio run with different set of endpoints", args.Addr))
		return
	}

	target := NewPeerRESTClientTarget(bucketName, args.TargetID, restClient)
	rulesMap := event.NewRulesMap(args.EventNames, args.Pattern, target.ID())
	if err := globalNotificationSys.AddRemoteTarget(bucketName, target, rulesMap); err != nil {
		reqInfo := &logger.ReqInfo{BucketName: target.bucketName}
		reqInfo.AppendTags("target", target.id.Name)
		ctx := logger.SetReqInfo(context.Background(), reqInfo)
		logger.LogIf(ctx, err)
		s.writeErrorResponse(w, err)
		return
	}
	w.(http.Flusher).Flush()
}

var errUnsupportedSignal = fmt.Errorf("unsupported signal: only restart and stop signals are supported")

// SignalServiceHandler - signal service handler.
func (s *peerRESTServer) SignalServiceHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}

	vars := mux.Vars(r)
	signalString := vars[peerRESTSignal]
	if signalString == "" {
		s.writeErrorResponse(w, errors.New("signal name is missing"))
		return
	}
	signal := serviceSignal(signalString)
	defer w.(http.Flusher).Flush()
	switch signal {
	case serviceRestart, serviceStop:
		globalServiceSignalCh <- signal
	default:
		s.writeErrorResponse(w, errUnsupportedSignal)
		return
	}
}

// TraceHandler sends http trace messages back to peer rest client
func (s *peerRESTServer) TraceHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("Invalid request"))
		return
	}
	trcAll := r.URL.Query().Get(peerRESTTraceAll) == "true"
	trcErr := r.URL.Query().Get(peerRESTTraceErr) == "true"

	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	doneCh := make(chan struct{})
	defer close(doneCh)

	// Trace Publisher uses nonblocking publish and hence does not wait for slow subscribers.
	// Use buffered channel to take care of burst sends or slow w.Write()
	ch := make(chan interface{}, 2000)

	globalHTTPTrace.Subscribe(ch, doneCh, func(entry interface{}) bool {
		return mustTrace(entry, trcAll, trcErr)
	})

	keepAliveTicker := time.NewTicker(500 * time.Millisecond)
	defer keepAliveTicker.Stop()

	enc := gob.NewEncoder(w)
	for {
		select {
		case entry := <-ch:
			if err := enc.Encode(entry); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		case <-keepAliveTicker.C:
			if err := enc.Encode(&trace.Info{}); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
}

func (s *peerRESTServer) BackgroundHealStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("invalid request"))
		return
	}

	ctx := newContext(r, w, "BackgroundHealStatus")

	state := getLocalBackgroundHealStatus()

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(state))
}

func (s *peerRESTServer) BackgroundOpsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		s.writeErrorResponse(w, errors.New("invalid request"))
		return
	}

	ctx := newContext(r, w, "BackgroundOpsStatus")

	state := BgOpsStatus{
		LifecycleOps: getLocalBgLifecycleOpsStatus(),
	}

	defer w.(http.Flusher).Flush()
	logger.LogIf(ctx, gob.NewEncoder(w).Encode(state))
}

func (s *peerRESTServer) writeErrorResponse(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(err.Error()))
}

// IsValid - To authenticate and verify the time difference.
func (s *peerRESTServer) IsValid(w http.ResponseWriter, r *http.Request) bool {
	if err := storageServerRequestValidate(r); err != nil {
		s.writeErrorResponse(w, err)
		return false
	}
	return true
}

// registerPeerRESTHandlers - register peer rest router.
func registerPeerRESTHandlers(router *mux.Router) {
	server := &peerRESTServer{}
	subrouter := router.PathPrefix(peerRESTPath).Subrouter()
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodNetReadPerfInfo).HandlerFunc(httpTraceHdrs(server.NetReadPerfInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodCollectNetPerfInfo).HandlerFunc(httpTraceHdrs(server.CollectNetPerfInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodGetLocks).HandlerFunc(httpTraceHdrs(server.GetLocksHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodServerInfo).HandlerFunc(httpTraceHdrs(server.ServerInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodCPULoadInfo).HandlerFunc(httpTraceHdrs(server.CPULoadInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodMemUsageInfo).HandlerFunc(httpTraceHdrs(server.MemUsageInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodDrivePerfInfo).HandlerFunc(httpTraceHdrs(server.DrivePerfInfoHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodDeleteBucket).HandlerFunc(httpTraceHdrs(server.DeleteBucketHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodSignalService).HandlerFunc(httpTraceHdrs(server.SignalServiceHandler)).Queries(restQueries(peerRESTSignal)...)

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketPolicyRemove).HandlerFunc(httpTraceAll(server.RemoveBucketPolicyHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketPolicySet).HandlerFunc(httpTraceHdrs(server.SetBucketPolicyHandler)).Queries(restQueries(peerRESTBucket)...)

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodDeletePolicy).HandlerFunc(httpTraceAll(server.LoadPolicyHandler)).Queries(restQueries(peerRESTPolicy)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodLoadPolicy).HandlerFunc(httpTraceAll(server.LoadPolicyHandler)).Queries(restQueries(peerRESTPolicy)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodLoadPolicyMapping).HandlerFunc(httpTraceAll(server.LoadPolicyMappingHandler)).Queries(restQueries(peerRESTUserOrGroup)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodDeleteUser).HandlerFunc(httpTraceAll(server.LoadUserHandler)).Queries(restQueries(peerRESTUser)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodLoadUser).HandlerFunc(httpTraceAll(server.LoadUserHandler)).Queries(restQueries(peerRESTUser, peerRESTUserTemp)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodLoadUsers).HandlerFunc(httpTraceAll(server.LoadUsersHandler))
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodLoadGroup).HandlerFunc(httpTraceAll(server.LoadGroupHandler)).Queries(restQueries(peerRESTGroup)...)

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodStartProfiling).HandlerFunc(httpTraceAll(server.StartProfilingHandler)).Queries(restQueries(peerRESTProfiler)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodDownloadProfilingData).HandlerFunc(httpTraceHdrs(server.DownloadProflingDataHandler))

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodTargetExists).HandlerFunc(httpTraceHdrs(server.TargetExistsHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodSendEvent).HandlerFunc(httpTraceHdrs(server.SendEventHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketNotificationPut).HandlerFunc(httpTraceHdrs(server.PutBucketNotificationHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketNotificationListen).HandlerFunc(httpTraceHdrs(server.ListenBucketNotificationHandler)).Queries(restQueries(peerRESTBucket)...)

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodReloadFormat).HandlerFunc(httpTraceHdrs(server.ReloadFormatHandler)).Queries(restQueries(peerRESTDryRun)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketLifecycleSet).HandlerFunc(httpTraceHdrs(server.SetBucketLifecycleHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBucketLifecycleRemove).HandlerFunc(httpTraceHdrs(server.RemoveBucketLifecycleHandler)).Queries(restQueries(peerRESTBucket)...)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBackgroundOpsStatus).HandlerFunc(server.BackgroundOpsStatusHandler)

	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodTrace).HandlerFunc(server.TraceHandler)
	subrouter.Methods(http.MethodPost).Path(SlashSeparator + peerRESTMethodBackgroundHealStatus).HandlerFunc(server.BackgroundHealStatusHandler)

	router.NotFoundHandler = http.HandlerFunc(httpTraceAll(notFoundHandler))
}
