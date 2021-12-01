// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Binary to run against a server to validate protocol conformance.
package main

import (
	"context"
	"crypto/tls"
	"fmt"

	"flag"
	"github.com/GoogleCloudPlatform/stet/client"
	"github.com/GoogleCloudPlatform/stet/constants"
	sspb "github.com/GoogleCloudPlatform/stet/proto/secure_session_go_proto"
	"github.com/GoogleCloudPlatform/stet/server"
	"github.com/GoogleCloudPlatform/stet/transportshim"
	"github.com/alecthomas/colour"
)

var (
	keyURI = flag.String("key-uri", fmt.Sprintf("http://localhost:%d/v0/%v", constants.HTTPPort, server.KeyPath1), "A valid key URI stored in the server")
)

const (
	recordHeaderHandshake      = 0x16
	handshakeHeaderServerHello = 0x02
)

type ekmClient struct {
	client client.ConfidentialEKMClient
	shim   transportshim.ShimInterface
	tls    *tls.Conn
}

// Initializes a new EKM client for the given version of TLS against the
// given key URL, also kicking off the internal TLS handshake.
func newEKMClient(keyURL string, tlsVersion int) ekmClient {
	c := ekmClient{}
	c.client = client.NewConfidentialEKMClient(keyURL)

	c.shim = transportshim.NewTransportShim()

	cfg := &tls.Config{
		CipherSuites:       constants.AllowableCipherSuites,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	}

	c.tls = tls.Client(c.shim, cfg)

	go func() {
		if err := c.tls.Handshake(); err != nil {
			return
		}
	}()

	return c
}

// Returns an empty byte array.
func emptyFn([]byte) []byte { return []byte{} }

// Given a byte array `b`, returns `b`.
func identityFn(b []byte) []byte { return b }

type beginSessionTest struct {
	testName         string
	expectErr        bool
	mutateTLSRecords func(r []byte) []byte
}

func runBeginSessionTestCase(mutateTLSRecords func(r []byte) []byte) error {
	ctx := context.Background()

	c := newEKMClient(*keyURI, tls.VersionTLS13)

	req := &sspb.BeginSessionRequest{
		TlsRecords: c.shim.DrainSendBuf(),
	}

	// Mutate the request TLS records.
	req.TlsRecords = mutateTLSRecords(req.TlsRecords)

	resp, err := c.client.BeginSession(ctx, req)
	if err != nil {
		return err
	}

	records := resp.GetTlsRecords()

	if records[0] != recordHeaderHandshake {
		return fmt.Errorf("Handshake record not received")
	}

	if records[5] != handshakeHeaderServerHello {
		return fmt.Errorf("Response is not Server Hello")
	}

	return nil
}

type handshakeTest struct {
	testName         string
	expectErr        bool
	mutateTLSRecords func(r []byte) []byte
	mutateSessionKey func(s []byte) []byte
}

func runHandshakeTestCase(mutateTLSRecords, mutateSessionKey func(r []byte) []byte) error {
	ctx := context.Background()

	c := newEKMClient(*keyURI, tls.VersionTLS13)

	req := &sspb.BeginSessionRequest{
		TlsRecords: c.shim.DrainSendBuf(),
	}

	resp, err := c.client.BeginSession(ctx, req)
	if err != nil {
		return err
	}

	sessionContext := mutateSessionKey(resp.GetSessionContext())
	c.shim.QueueReceiveBuf(resp.GetTlsRecords())

	req2 := &sspb.HandshakeRequest{
		SessionContext: sessionContext,
		TlsRecords:     mutateTLSRecords(c.shim.DrainSendBuf()),
	}

	_, err = c.client.Handshake(ctx, req2)
	if err != nil {
		return err
	}

	// Under TLS 1.3, the TLS implementation has nothing to return here.
	// However, attempting to call `c.tls.ConnectionState()` when the
	// server communicates with TLS 1.2 causes the client to hang
	// infinitely, so as a proxy, perform checks on the response records
	// only if they are non-nil.
	if len(resp.GetTlsRecords()) > 0 {
		records := resp.GetTlsRecords()

		// The handshake data itself is encypted, so just verify that the
		// header for this segment of data is a handshake record.
		if records[0] != recordHeaderHandshake {
			return fmt.Errorf("Handshake record not received")
		}
	}

	return nil
}

func main() {
	// Define and run BeginSession tests.
	fmt.Println("Running BeginSession tests...")

	testCases := []beginSessionTest{
		{
			testName:         "Valid request with proper TLS Client Hello",
			expectErr:        false,
			mutateTLSRecords: identityFn,
		},
		{
			testName:  "Malformed Client Hello in request",
			expectErr: true,
			mutateTLSRecords: func(r []byte) []byte {
				r[5] = 0xFF // Client Hello byte should be 0x01
				return r
			},
		},
		{
			testName:         "No TLS records in request",
			expectErr:        true,
			mutateTLSRecords: emptyFn,
		},
	}

	for _, testCase := range testCases {
		err := runBeginSessionTestCase(testCase.mutateTLSRecords)
		testPassed := testCase.expectErr == (err != nil)
		if testPassed {
			colour.Printf(" - ^2%v^R\n", testCase.testName)
		} else {
			colour.Printf(" - ^1%v^R (%v)\n", testCase.testName, err.Error())
		}
	}

	// Define and run Handshake tests.
	fmt.Println("Running Handshake tests...")

	testCases2 := []handshakeTest{
		{
			testName:         "Valid request with proper TLS Client Handshake",
			expectErr:        false,
			mutateTLSRecords: identityFn,
			mutateSessionKey: identityFn,
		},
		{
			testName:         "No TLS records in request",
			expectErr:        true,
			mutateTLSRecords: emptyFn,
			mutateSessionKey: identityFn,
		},
		{
			testName:         "Invalid session key",
			expectErr:        true,
			mutateTLSRecords: identityFn,
			mutateSessionKey: emptyFn,
		},
	}

	for _, testCase := range testCases2 {
		err := runHandshakeTestCase(testCase.mutateTLSRecords, testCase.mutateSessionKey)
		testPassed := testCase.expectErr == (err != nil)
		if testPassed {
			colour.Printf(" - ^2%v^R\n", testCase.testName)
		} else {
			colour.Printf(" - ^1%v^R (%v)\n", testCase.testName, err.Error())
		}
	}
}
