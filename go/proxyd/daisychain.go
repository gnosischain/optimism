package proxyd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
)

// TODO: should get by hash work its way backwards
// until it finds the value?

type DaisyChainServer struct {
	rpcServer    *http.Server
	wsServer     *http.Server
	maxBodySize  int64
	epoch1RPCURL string
	epoch2RPCURL string
	epoch3RPCURL string
	epoch4RPCURL string
	epoch5RPCURL string
	epoch6RPCURL string
	client       *http.Client
}

// TODO: support "latest" for epoch
type RequestOptions struct {
	Epoch *uint `json:"epoch,omitempty"`
}

var latestEpoch = uint(6)

// TODO: also add in debug methods
var argTypes = map[string][]reflect.Type{
	"eth_blockNumber": []reflect.Type{
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getBalance": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf(rpc.BlockNumberOrHash{}),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getProof": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf([]string{}),
		reflect.TypeOf(rpc.BlockNumberOrHash{}),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getHeaderByNumber": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getBlockByNumber": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(true),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getUncleByBlockNumberAndIndex": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(hexutil.Uint(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getUncleCountByBlockNumber": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getCode": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getStorageAt": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf(""),
		reflect.TypeOf(rpc.BlockNumberOrHash{}),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_call": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf(TransactionArgs{}),
		reflect.TypeOf(rpc.BlockNumberOrHash{}),
		reflect.TypeOf(&StateOverride{}),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getBlockTransactionCountByNumber": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getTransactionByBlockNumberAndIndex": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(hexutil.Uint(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getRawTransactionByBlockNumberAndIndex": []reflect.Type{
		reflect.TypeOf(rpc.BlockNumber(0)),
		reflect.TypeOf(hexutil.Uint(0)),
		reflect.TypeOf(&RequestOptions{}),
	},
	"eth_getTransactionCount": []reflect.Type{
		reflect.TypeOf(common.Address{}),
		reflect.TypeOf(rpc.BlockNumberOrHash{}),
		reflect.TypeOf(&RequestOptions{}),
	},
}

// mapping of method to index of hash in rpc params
var hashMethods = map[string]int{
	"eth_getBalance":                           1,
	"eth_getProof":                             2,
	"eth_getBlockByHash":                       0,
	"eth_getUncleByBlockHashAndIndex":          0,
	"eth_getUncleCountByBlockHash":             0,
	"eth_getCode":                              1,
	"eth_getStorageAt":                         2,
	"eth_call":                                 1,
	"eth_estimateGas":                          2,
	"eth_getBlockTransactionCountByHash":       0,
	"eth_getTransactionByBlockHashAndIndex":    0,
	"eth_getRawTransactionByBlockHashAndIndex": 0,
	"eth_getTransactionCount":                  1,
	"eth_getTransactionByHash":                 0,
	"eth_getRawTransactionByHash":              0,
	"eth_getTransactionReceipt":                0,
}

func NewDaisyChainServer(urls []string) *DaisyChainServer {
	if len(urls) != 6 {
		panic("must pass 6 urls")
	}

	srv := DaisyChainServer{
		maxBodySize:  100_000, // TODO: from config
		epoch1RPCURL: urls[0],
		epoch2RPCURL: urls[1],
		epoch3RPCURL: urls[2],
		epoch4RPCURL: urls[3],
		epoch5RPCURL: urls[4],
		epoch6RPCURL: urls[5],
	}

	srv.client = &http.Client{
		Timeout: 5 * time.Second,
	}

	return &srv
}

func StartDaisyChain(config *DaisyChainConfig) (func(), error) {
	epoch1RPCURL := config.Backends.Epoch1RPCURL
	epoch2RPCURL := config.Backends.Epoch2RPCURL
	epoch3RPCURL := config.Backends.Epoch3RPCURL
	epoch4RPCURL := config.Backends.Epoch4RPCURL
	epoch5RPCURL := config.Backends.Epoch5RPCURL
	epoch6RPCURL := config.Backends.Epoch6RPCURL

	urls := []string{
		epoch1RPCURL,
		epoch2RPCURL,
		epoch3RPCURL,
		epoch4RPCURL,
		epoch5RPCURL,
		epoch6RPCURL,
	}

	defined := false
	for i, url := range urls {
		if url != "" {
			log.Info("epoch rpc url defined", "epoch", i+1)
			defined = true
		}
	}
	if !defined {
		panic("must define one epoch url")
	}

	// parse the config
	srv := NewDaisyChainServer(urls)

	if config.Metrics.Enabled {
		addr := fmt.Sprintf("%s:%d", config.Metrics.Host, config.Metrics.Port)
		log.Info("starting metrics server", "addr", addr)
		go http.ListenAndServe(addr, promhttp.Handler())
	}

	// To allow integration tests to cleanly come up, wait
	// 10ms to give the below goroutines enough time to
	// encounter an error creating their servers
	errTimer := time.NewTimer(10 * time.Millisecond)

	if config.Server.RPCPort != 0 {
		go func() {
			if err := srv.RPCListenAndServe(config.Server.RPCHost, config.Server.RPCPort); err != nil {
				if errors.Is(err, http.ErrServerClosed) {
					log.Info("RPC server shut down")
					return
				}
				log.Crit("error starting RPC server", "err", err)
			}
		}()
	}

	if config.Server.WSPort != 0 {
		go func() {
			if err := srv.WSListenAndServe(config.Server.WSHost, config.Server.WSPort); err != nil {
				if errors.Is(err, http.ErrServerClosed) {
					log.Info("WS server shut down")
					return
				}
				log.Crit("error starting WS server", "err", err)
			}
		}()
	}

	<-errTimer.C
	log.Info("started daisychain")

	return func() {
		log.Info("shutting down daisychain")
		srv.Shutdown()
		log.Info("goodbye")
	}, nil
}

// TODO: batch support
func (s *DaisyChainServer) HandleRPC(w http.ResponseWriter, r *http.Request) {
	ctx := s.populateContext(w, r)
	if ctx == nil {
		return
	}

	log.Info(
		"received RPC request",
		"req_id", GetReqID(ctx),
		"auth", GetAuthCtx(ctx),
		"user_agent", r.Header.Get("user-agent"),
	)

	body, err := ioutil.ReadAll(io.LimitReader(r.Body, s.maxBodySize))
	if err != nil {
		log.Error("error reading request body", "err", err)
		writeRPCError(ctx, w, nil, ErrInternal)
		return
	}
	RecordRequestPayloadSize(ctx, len(body))

	req, err := ParseRPCReq(body)
	if err != nil {
		log.Info("error parsing RPC call", "source", "rpc", "err", err)
		writeRPCError(ctx, w, nil, err)
		return
	}

	// if the method is in the hash type map and
	// the item in the params is a hash
	if index, ok := hashMethods[req.Method]; ok {
		// TODO: argTypes doesn't have all required entries
		argType, ok := argTypes[req.Method]
		if !ok {
			fmt.Println("ERROR")
			return
		}

		values, err := parsePositionalArguments(req.Params, argType)
		if err != nil {
			res := NewRPCErrorRes(nil, err)
			writeRPCRes(ctx, w, res)
			return
		}
		param, ok := values[index].Interface().(rpc.BlockNumberOrHash)
		if !ok {
			writeRPCError(ctx, w, nil, errors.New(""))
			return
		}
		if _, ok := param.Hash(); ok {
			backendRes := s.handleHashTaggedRPC(ctx, req)
			writeRPCRes(ctx, w, backendRes)
			return
		}
	}

	// See if the incoming method is in the list of methods
	// that need to be proxied by observing the request options
	argType, ok := argTypes[req.Method]
	if !ok {
		// The request can be forwarded to the most recent node.
		// TODO: finalize this functionality, because it could
		// be forward to one of the two most recent nodes, if the
		// most recent node doesn't start with a 0 blocknumber
		// We haven't completely derisked the diff to geth it
		// is to have non contiguous block data
		if s.epoch6RPCURL == "" {
			log.Error("Request for latest epoch cannot be handled")
			writeRPCError(ctx, w, nil, errors.New("must configure epoch 6 url"))
			return
		}

		backendRes := s.handleSingleRPC(ctx, s.epoch6RPCURL, req)
		writeRPCRes(ctx, w, backendRes)
		return
	}

	values, err := parsePositionalArguments(req.Params, argType)
	if err != nil {
		log.Error("Cannot parse JSON RPC arguments")
		res := NewRPCErrorRes(nil, err)
		writeRPCRes(ctx, w, res)
		return
	}

	// The final arg should be a *RequestOptions
	finalArg := values[len(values)-1]
	// Double check that it is the correct type
	argument, ok := finalArg.Interface().(*RequestOptions)
	if !ok {
		writeRPCError(ctx, w, nil, errors.New("unknown rpc param"))
		return
	}

	// When the final argument is not passed, forward
	// to the latest
	if argument == nil {
		if s.epoch6RPCURL == "" {
			writeRPCError(ctx, w, nil, errors.New("must configure epoch 6 url"))
			return
		}

		// TODO: this may need to go to 5 and 6 depending on a height
		// if we decide to start epoch 6 at non zero block number
		backendRes := s.handleSingleRPC(ctx, s.epoch6RPCURL, req)
		writeRPCRes(ctx, w, backendRes)
		return
	}

	// If the epoch is not set, default to the latest
	if argument.Epoch == nil {
		argument.Epoch = &latestEpoch
	}

	url := ""
	switch *argument.Epoch {
	case 6:
		url = s.epoch6RPCURL
	case 5:
		url = s.epoch5RPCURL
	case 4:
		url = s.epoch4RPCURL
	case 3:
		url = s.epoch3RPCURL
	case 2:
		url = s.epoch2RPCURL
	case 1:
		url = s.epoch1RPCURL
	default:
		writeRPCError(ctx, w, nil, errors.New("bad epoch"))
		return
	}

	// TODO: delete this so a url with a key doesn't leak
	log.Info("Sending rpc req", "url", url)

	if url == "" {
		writeRPCError(ctx, w, nil, errors.New("epoch not configured"))
		return
	}

	// There should never be an empty params by this point
	raw, err := json.Marshal(values[0 : len(values)-1])
	if err != nil {
		writeRPCError(ctx, w, nil, errors.New("cannot serialize json"))
		return
	}

	req.Params = raw
	backendRes := s.handleSingleRPC(ctx, url, req)
	writeRPCRes(ctx, w, backendRes)
}

func (s *DaisyChainServer) RPCListenAndServe(host string, port int) error {
	hdlr := mux.NewRouter()
	hdlr.HandleFunc("/healthz", s.HandleHealthz).Methods("GET")
	hdlr.HandleFunc("/", s.HandleRPC).Methods("POST")
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
	})
	addr := fmt.Sprintf("%s:%d", host, port)
	s.rpcServer = &http.Server{
		Handler: instrumentedHdlr(c.Handler(hdlr)),
		Addr:    addr,
	}
	log.Info("starting HTTP server", "addr", addr)
	return s.rpcServer.ListenAndServe()
}

func (s *DaisyChainServer) WSListenAndServe(host string, port int) error {
	hdlr := mux.NewRouter()
	hdlr.HandleFunc("/healthz", s.HandleHealthz).Methods("GET")
	// TODO: fix
	//hdlr.HandleFunc("/", s.HandleWS)
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
	})
	addr := fmt.Sprintf("%s:%d", host, port)
	s.wsServer = &http.Server{
		Handler: instrumentedHdlr(c.Handler(hdlr)),
		Addr:    addr,
	}
	log.Info("starting WS server", "addr", addr)
	return s.wsServer.ListenAndServe()
}

func (s *DaisyChainServer) Shutdown() {
	if s.rpcServer != nil {
		s.rpcServer.Shutdown(context.Background())
	}
	if s.wsServer != nil {
		s.wsServer.Shutdown(context.Background())
	}
}

func (s *DaisyChainServer) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

func (s *DaisyChainServer) populateContext(w http.ResponseWriter, r *http.Request) context.Context {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		ipPort := strings.Split(r.RemoteAddr, ":")
		if len(ipPort) == 2 {
			xff = ipPort[0]
		}
	}

	ctx := context.WithValue(r.Context(), ContextKeyXForwardedFor, xff)
	return context.WithValue(
		ctx,
		ContextKeyReqID,
		randStr(10),
	)
}

func (s *DaisyChainServer) handleSingleRPC(ctx context.Context, url string, req *RPCReq) *RPCRes {
	if url == "" {
		return NewRPCErrorRes(nil, errors.New("no backend url"))
	}

	if err := ValidateRPCReq(req); err != nil {
		RecordRPCError(ctx, BackendProxyd, MethodUnknown, err)
		return NewRPCErrorRes(nil, err)
	}

	body := mustMarshalJSON(req)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return NewRPCErrorRes(req.ID, err)
	}

	httpReq.Header.Set("content-type", "application/json")
	httpRes, err := s.client.Do(httpReq)
	if err != nil {
		return NewRPCErrorRes(req.ID, err)
	}

	defer httpRes.Body.Close()
	resB, err := ioutil.ReadAll(io.LimitReader(httpRes.Body, s.maxBodySize))

	backendRes := new(RPCRes)
	if err := json.Unmarshal(resB, backendRes); err != nil {
		return NewRPCErrorRes(req.ID, err)
	}

	return backendRes
}

// Tries each rpc url one after another
func (s *DaisyChainServer) handleHashTaggedRPC(ctx context.Context, req *RPCReq) *RPCRes {
	urls := []string{
		s.epoch6RPCURL,
		s.epoch5RPCURL,
		s.epoch4RPCURL,
		s.epoch3RPCURL,
		s.epoch2RPCURL,
		s.epoch1RPCURL,
	}

	var res *RPCRes
	for _, url := range urls {
		res = s.handleSingleRPC(ctx, url, req)
		if !res.IsError() {
			break
		}
	}
	return res
}

// parsePositionalArguments tries to parse the given args to an array of values with the
// given types. It returns the parsed values or an error when the args could not be
// parsed. Missing optional arguments are returned as reflect.Zero values.
func parsePositionalArguments(rawArgs json.RawMessage, types []reflect.Type) ([]reflect.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(rawArgs))
	var args []reflect.Value
	tok, err := dec.Token()
	switch {
	case err == io.EOF || tok == nil && err == nil:
		// "params" is optional and may be empty. Also allow "params":null even though it's
		// not in the spec because our own client used to send it.
	case err != nil:
		return nil, err
	case tok == json.Delim('['):
		// Read argument array.
		if args, err = parseArgumentArray(dec, types); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("non-array args")
	}
	// Set any missing args to nil.
	for i := len(args); i < len(types); i++ {
		if types[i].Kind() != reflect.Ptr {
			return nil, fmt.Errorf("missing value for required argument %d", i)
		}
		args = append(args, reflect.Zero(types[i]))
	}
	return args, nil
}

func parseArgumentArray(dec *json.Decoder, types []reflect.Type) ([]reflect.Value, error) {
	args := make([]reflect.Value, 0, len(types))
	for i := 0; dec.More(); i++ {
		if i >= len(types) {
			return args, fmt.Errorf("too many arguments, want at most %d", len(types))
		}
		argval := reflect.New(types[i])
		if err := dec.Decode(argval.Interface()); err != nil {
			return args, fmt.Errorf("invalid argument %d: %v", i, err)
		}
		if argval.IsNil() && types[i].Kind() != reflect.Ptr {
			return args, fmt.Errorf("missing value for required argument %d", i)
		}
		args = append(args, argval.Elem())
	}
	// Read end of args array.
	_, err := dec.Token()
	return args, err
}

// TransactionArgs represents the arguments to construct a new transaction
// or a message call.
type TransactionArgs struct {
	From                 *common.Address `json:"from"`
	To                   *common.Address `json:"to"`
	Gas                  *hexutil.Uint64 `json:"gas"`
	GasPrice             *hexutil.Big    `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big    `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big    `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big    `json:"value"`
	Nonce                *hexutil.Uint64 `json:"nonce"`

	// We accept "data" and "input" for backwards-compatibility reasons.
	// "input" is the newer name and should be preferred by clients.
	// Issue detail: https://github.com/ethereum/go-ethereum/issues/15628
	Data  *hexutil.Bytes `json:"data"`
	Input *hexutil.Bytes `json:"input"`

	// Introduced by AccessListTxType transaction.
	AccessList *types.AccessList `json:"accessList,omitempty"`
	ChainID    *hexutil.Big      `json:"chainId,omitempty"`
}

// OverrideAccount indicates the overriding fields of account during the execution
// of a message call.
// Note, state and stateDiff can't be specified at the same time. If state is
// set, message execution will only use the data in the given state. Otherwise
// if statDiff is set, all diff will be applied first and then execute the call
// message.
type OverrideAccount struct {
	Nonce     *hexutil.Uint64              `json:"nonce"`
	Code      *hexutil.Bytes               `json:"code"`
	Balance   **hexutil.Big                `json:"balance"`
	State     *map[common.Hash]common.Hash `json:"state"`
	StateDiff *map[common.Hash]common.Hash `json:"stateDiff"`
}

// StateOverride is the collection of overridden accounts.
type StateOverride map[common.Address]OverrideAccount
