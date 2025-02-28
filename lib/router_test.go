package lib

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockHTTPServer struct {
	t               *testing.T
	statusCode      int
	expectedRequest string
	response        string
	reqCount        int
	shouldError     bool
}

func (m *mockHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.shouldError {
		w.WriteHeader(200)
		resp, err := formatErrorResponse("errored intentionally for test")
		require.Nil(m.t, err, "error formatting error")
		w.Write(resp)
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	require.Nil(m.t, err, "error reading body")

	assert.JSONEq(m.t, m.expectedRequest, string(body), "expected json body to be equal")

	w.WriteHeader(m.statusCode)
	w.Write([]byte(m.response))
	m.reqCount++
}

func newMockHTTPServer(t *testing.T, statusCode int, expectedRequest string, response string, shouldError bool) (*mockHTTPServer, *httptest.Server) {
	server := &mockHTTPServer{
		t:               t,
		statusCode:      statusCode,
		expectedRequest: expectedRequest,
		response:        response,
		shouldError:     shouldError,
	}

	return server, httptest.NewServer(server)
}

func TestNewRouter(t *testing.T) {
	_, mockHTTPServer := newMockHTTPServer(t, 200, "", "{}", false)

	tests := []struct {
		name      string
		relayURLs []string
		wantErr   bool
	}{
		{
			"success",
			[]string{"http://bar"},
			false,
		},
		{
			"MockHTTPServer success",
			[]string{mockHTTPServer.URL},
			false,
		},
		{
			"fails with empty relayURL",
			[]string{""},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRouter(tt.relayURLs, NewStore(), logrus.WithField("testing", true))
			if (err != nil) != tt.wantErr {
				t.Errorf("NewRouter() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func formatRequestBody(method string, params []interface{}) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"id":      "1",
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func formatResponse(responseResult interface{}) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"error":   nil,
		"result":  responseResult,
	})
}

func formatErrorResponse(err string) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"error":   map[string]interface{}{"code": -32000, "message": err},
	})
}

type httpTest struct {
	name                    string
	requestArray            []interface{}
	expectedResponseResult  interface{}
	expectedResponseCheck   func(t *testing.T, rpcResp *rpcResponse) // if expectedResponseCheck is provided, expectedResponseResult will be ignored
	expectedStatusCode      int
	mockStatusCode          int
	expectedRequestsToRelay int
	errorRelay              bool
}

type httpTestWithMethods struct {
	httpTest

	jsonRPCMethodCaller     string
	jsonRPCMethodRelayProxy string
	skipRespCheck           bool
}

func testHTTPMethod(t *testing.T, jsonRPCMethod string, tt *httpTest) {
	testHTTPMethodWithDifferentRPC(t, jsonRPCMethod, jsonRPCMethod, tt, false, nil)
}

func testHTTPMethodWithDifferentRPC(t *testing.T, jsonRPCMethodCaller string, jsonRPCMethodRelay string, tt *httpTest, skipRespCheck bool, store Store) {
	t.Run(tt.name, func(t *testing.T) {
		// Format JSON-RPC body with the provided method and array of args
		body, err := formatRequestBody(jsonRPCMethodCaller, tt.requestArray)
		require.Nil(t, err, "error formatting json body")
		bodyRelayProxy, err := formatRequestBody(jsonRPCMethodRelay, tt.requestArray)
		require.Nil(t, err, "error formatting json body")

		// Format JSON-RPC response
		resp, err := formatResponse(tt.expectedResponseResult)
		require.Nil(t, err, "error formatting json response")

		// Create mock http server that expects the above bodyProxy and returns the above response
		mockRelay, mockRelayHTTP := newMockHTTPServer(t, tt.mockStatusCode, string(bodyRelayProxy), string(resp), tt.errorRelay)

		if store == nil {
			store = NewStore()
			store.SetForkchoiceResponse("0x01", mockRelayHTTP.URL, "0x01")
		}

		// Create the router pointing at the mock server
		r, err := NewRouter([]string{mockRelayHTTP.URL}, store, logrus.WithField("testing", true))
		require.Nil(t, err, "error creating router")

		// Craft a JSON-RPC request to the router
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Add("Content-Type", "application/json")
		w := httptest.NewRecorder()

		// Actually send the request, testing the router
		r.ServeHTTP(w, req)

		if !skipRespCheck {
			if tt.expectedResponseCheck != nil {
				rpcResp, err := parseRPCResponse(w.Body.Bytes())
				require.Nil(t, err, "error parsing rpc response")
				tt.expectedResponseCheck(t, rpcResp)
			} else {
				assert.JSONEq(t, string(resp), w.Body.String(), "expected response to be json equal")
			}
		}
		assert.Equal(t, tt.expectedStatusCode, w.Result().StatusCode, "expected status code to be equal")
		assert.Equal(t, tt.expectedRequestsToRelay, mockRelay.reqCount, "expected request count to relay to be equal")
	})
}

func strToBytes(s string) *hexutil.Bytes {
	ret := hexutil.Bytes(common.Hex2Bytes(s))
	return &ret
}

func TestStrToBytes(t *testing.T) {
	a := strToBytes("0x1")
	b := strToBytes("0x01")
	require.Equal(t, a, b)
}

func TestMevService_ForkChoiceUpdated(t *testing.T) {
	tests := []httpTest{
		{
			"basic success",
			[]interface{}{catalyst.ForkchoiceStateV1{}, catalyst.PayloadAttributesV1{
				SuggestedFeeRecipient: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			}},
			ForkChoiceResponse{PayloadID: strToBytes("0x1"), PayloadStatus: PayloadStatus{Status: ForkchoiceStatusValid}},
			func(t *testing.T, rpcResp *rpcResponse) {
				var resp ForkChoiceResponse
				err := json.Unmarshal(rpcResp.Result, &resp)
				require.Nil(t, err, err)
				assert.Equal(t, 8, len(*resp.PayloadID))
				assert.Equal(t, ForkchoiceStatusValid, resp.PayloadStatus.Status)
			},
			200,
			200,
			1,
			false,
		},
	}
	for _, tt := range tests {
		testHTTPMethod(t, "engine_forkchoiceUpdatedV1", &tt)
	}
}

func TestRelayService_ProposeBlindedBlockV1(t *testing.T) {
	tests := []httpTest{
		{
			"basic success",
			[]interface{}{SignedBlindedBeaconBlock{
				Message: &BlindedBeaconBlock{
					ParentRoot: "0x0000000000000000000000000000000000000000000000000000000000000001",
				},
				Signature: "0x0000000000000000000000000000000000000000000000000000000000000002",
			}},

			ExecutionPayloadWithTxRootV1{
				BlockHash:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
				BaseFeePerGas:    big.NewInt(4),
				Transactions:     &[]string{},
				FeeRecipientDiff: big.NewInt(0),
			},
			nil,
			200,
			200,
			1,
			false,
		},
	}
	for _, tt := range tests {
		testHTTPMethodWithDifferentRPC(t, "builder_proposeBlindedBlockV1", "relay_proposeBlindedBlockV1", &tt, false, nil)
	}
}

func TestRelayService_GetPayloadHeaderV1(t *testing.T) {
	tests := []httpTest{
		{
			"basic success",
			[]interface{}{"0x01"},
			ExecutionPayloadWithTxRootV1{
				BlockHash:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
				BaseFeePerGas:    big.NewInt(4),
				TransactionsRoot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000002"),
				FeeRecipientDiff: big.NewInt(0),
			},
			nil,
			200,
			200,
			1,
			false,
		},
	}
	for _, tt := range tests {
		testHTTPMethodWithDifferentRPC(t, "builder_getPayloadHeaderV1", "relay_getPayloadHeaderV1", &tt, false, nil)
	}
}

func TestRelayService_GetPayloadAndPropose(t *testing.T) {
	store := NewStore()

	payload := ExecutionPayloadWithTxRootV1{
		BlockHash:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		StateRoot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000003"),
		BaseFeePerGas:    big.NewInt(4),
		Transactions:     &[]string{},
		TransactionsRoot: common.HexToHash("0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"),
		FeeRecipientDiff: big.NewInt(0),
	}
	payloadBytes, err := json.Marshal(payload)
	// make block_hash be snake_case
	payloadBytes = []byte(strings.Replace(string(payloadBytes), "blockHash", "block_hash", -1))
	require.Nil(t, err)

	tests := []httpTestWithMethods{
		{
			httpTest{
				"get payload and store it",
				[]interface{}{"0x01"},
				payload,
				nil,
				200,
				200,
				0,
				true,
			},
			"builder_getPayloadHeaderV1",
			"relay_getPayloadHeaderV1",
			true, // this endpoint transforms Transactions into TransactionsRoot, so skip equality check
		},
		{
			httpTest{
				"block cache hit",
				[]interface{}{SignedBlindedBeaconBlock{
					Message: &BlindedBeaconBlock{
						ParentRoot: "0x0000000000000000000000000000000000000000000000000000000000000001",
						StateRoot:  "0x0000000000000000000000000000000000000000000000000000000000000003",
						Body:       []byte(`{"execution_payload_header": ` + string(payloadBytes) + `}`),
					},
					Signature: "0x0000000000000000000000000000000000000000000000000000000000000002",
				}},
				payload,
				nil,
				200,
				200,
				1,
				false,
			},
			"builder_proposeBlindedBlockV1",
			"relay_proposeBlindedBlockV1",
			false,
		},
	}
	for _, tt := range tests {
		testHTTPMethodWithDifferentRPC(t, tt.jsonRPCMethodCaller, tt.jsonRPCMethodRelayProxy, &tt.httpTest, tt.skipRespCheck, store)
	}
}

func TestRelayService_GetPayloadAndProposeCamelCase(t *testing.T) {
	store := NewStore()

	payload := ExecutionPayloadWithTxRootV1{
		BlockHash:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
		StateRoot:        common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000003"),
		BaseFeePerGas:    big.NewInt(4),
		Transactions:     &[]string{},
		TransactionsRoot: common.HexToHash("0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"),
		FeeRecipientDiff: big.NewInt(0),
	}
	payloadBytes, err := json.Marshal(payload)
	require.Nil(t, err)

	tests := []httpTestWithMethods{
		{
			httpTest{
				"get payload and store it",
				[]interface{}{"0x1"},
				payload,
				nil,
				200,
				200,
				0,
				true,
			},
			"builder_getPayloadHeaderV1",
			"relay_getPayloadHeaderV1",
			true, // this endpoint transforms Transactions into TransactionsRoot, so skip equality check
		},
		{
			httpTest{
				"block cache hit",
				[]interface{}{SignedBlindedBeaconBlock{
					Message: &BlindedBeaconBlock{
						ParentRoot: "0x0000000000000000000000000000000000000000000000000000000000000001",
						StateRoot:  "0x0000000000000000000000000000000000000000000000000000000000000003",
						Body:       []byte(`{"executionPayloadHeader": ` + string(payloadBytes) + `}`),
					},
					Signature: "0x0000000000000000000000000000000000000000000000000000000000000002",
				}},
				payload,
				nil,
				200,
				200,
				1,
				false,
			},
			"builder_proposeBlindedBlockV1",
			"relay_proposeBlindedBlockV1",
			false,
		},
	}
	for _, tt := range tests {
		testHTTPMethodWithDifferentRPC(t, tt.jsonRPCMethodCaller, tt.jsonRPCMethodRelayProxy, &tt.httpTest, tt.skipRespCheck, store)
	}
}
