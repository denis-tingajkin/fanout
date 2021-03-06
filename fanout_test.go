// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fanout

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

const testQuery = "example1."

type cachedDNSWriter struct {
	answers []*dns.Msg
	mutex   sync.Mutex
	*test.ResponseWriter
}

func (w *cachedDNSWriter) WriteMsg(m *dns.Msg) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.answers = append(w.answers, m)
	return w.ResponseWriter.WriteMsg(m)
}

type server struct {
	addr  string
	inner *dns.Server
}

func (s *server) close() {
	logErrIfNotNil(s.inner.Shutdown())
}

func newServer(f dns.HandlerFunc) *server {
	ch := make(chan bool)
	s := &dns.Server{}
	s.Handler = f

	for i := 0; i < 10; i++ {
		s.Listener, _ = net.Listen(tcp, ":0")
		if s.Listener != nil {
			break
		}
	}
	if s.Listener == nil {
		panic("failed to create new client")
	}

	s.NotifyStartedFunc = func() { close(ch) }
	go func() {
		logErrIfNotNil(s.ActivateAndServe())
	}()

	<-ch
	return &server{inner: s, addr: s.Listener.Addr().String()}
}

func makeRecordA(rr string) *dns.A {
	r, _ := dns.NewRR(rr)
	return r.(*dns.A)
}

func TestFanoutCanReturnUnsuccessRespnse(t *testing.T) {
	s := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		msg := nxdomainMsg()
		msg.SetRcode(r, msg.Rcode)
		logErrIfNotNil(w.WriteMsg(msg))
	})
	f := New()
	f.from = "."
	c := NewClient(s.addr, "tcp")
	f.addClient(c)
	req := new(dns.Msg)
	req.SetQuestion(testQuery, dns.TypeA)
	writer := &cachedDNSWriter{ResponseWriter: new(test.ResponseWriter)}
	_, err := f.ServeDNS(context.TODO(), writer, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writer.answers) != 1 {
		t.FailNow()
	}
	if writer.answers[0].MsgHdr.Rcode != dns.RcodeNameError {
		t.Error("fanout plugin returns first negative answer if other answers on request are negative")
	}
}
func TestFanoutTwoServersNotSuccessResponse(t *testing.T) {
	rcode := 1
	rcodeMutex := sync.Mutex{}
	s1 := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		if r.Question[0].Name == testQuery {
			msg := nxdomainMsg()
			rcodeMutex.Lock()
			msg.SetRcode(r, rcode)
			rcode++
			rcode %= dns.RcodeNotZone
			rcodeMutex.Unlock()
			logErrIfNotNil(w.WriteMsg(msg))
		}
	})
	s2 := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		if r.Question[0].Name == testQuery {
			msg := dns.Msg{
				Answer: []dns.RR{makeRecordA("example1. 3600	IN	A 10.0.0.1")},
			}
			msg.SetReply(r)
			logErrIfNotNil(w.WriteMsg(&msg))
		}
	})
	defer s1.close()
	defer s2.close()
	c1 := NewClient(s1.addr, "tcp")
	c2 := NewClient(s2.addr, "tcp")
	f := New()
	f.from = "."
	f.addClient(c1)
	f.addClient(c2)
	writer := &cachedDNSWriter{ResponseWriter: new(test.ResponseWriter)}
	for i := 0; i < 10; i++ {
		req := new(dns.Msg)
		req.SetQuestion(testQuery, dns.TypeA)
		_, err := f.ServeDNS(context.TODO(), writer, req)
		if err != nil {
			t.Fatal(err.Error())
		}
	}
	for _, m := range writer.answers {
		if m.MsgHdr.Rcode != dns.RcodeSuccess {
			t.Error("fanout should return only positive answers")
		}
	}
}

func TestFanoutTwoServers(t *testing.T) {
	const expected = 1
	var mutex sync.Mutex
	answerCount1 := 0
	answerCount2 := 0
	s1 := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		if r.Question[0].Name == testQuery {
			msg := dns.Msg{
				Answer: []dns.RR{makeRecordA("example1 3600	IN	A 10.0.0.1")},
			}
			mutex.Lock()
			answerCount1++
			mutex.Unlock()
			msg.SetReply(r)
			logErrIfNotNil(w.WriteMsg(&msg))
		}
	})
	s2 := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		if r.Question[0].Name == "example2." {
			msg := dns.Msg{
				Answer: []dns.RR{makeRecordA("example2. 3600	IN	A 10.0.0.1")},
			}
			mutex.Lock()
			answerCount2++
			mutex.Unlock()
			msg.SetReply(r)
			logErrIfNotNil(w.WriteMsg(&msg))
		}
	})
	defer s1.close()
	defer s2.close()

	c1 := NewClient(s1.addr, "tcp")
	c2 := NewClient(s2.addr, "tcp")
	f := New()
	f.from = "."
	f.addClient(c1)
	f.addClient(c2)

	req := new(dns.Msg)
	req.SetQuestion(testQuery, dns.TypeA)
	_, err := f.ServeDNS(context.TODO(), &test.ResponseWriter{}, req)
	if err != nil {
		t.Fatal(err.Error())
	}
	<-time.After(time.Second)
	req = new(dns.Msg)
	req.SetQuestion("example2.", dns.TypeA)
	_, err = f.ServeDNS(context.TODO(), &test.ResponseWriter{}, req)
	if err != nil {
		t.Fatal(err.Error())
	}
	mutex.Lock()
	defer mutex.Unlock()
	if answerCount2 != expected || answerCount1 != expected {
		t.Errorf("Expected number of health checks to be %d, got s1: %d, s2: %d", expected, answerCount1, answerCount2)
	}
}

func TestFanouWorkerCountLessThenServers(t *testing.T) {
	const expected = 1
	answerCount1 := 0
	var mutex sync.Mutex
	var closeFuncs []func()
	free := func() {
		for _, f := range closeFuncs {
			f()
		}
	}
	defer free()
	f := New()
	f.from = "."

	for i := 0; i < 4; i++ {
		incorrectServer := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		})
		f.addClient(NewClient(incorrectServer.addr, "tcp"))
		closeFuncs = append(closeFuncs, incorrectServer.close)
	}
	correctServer := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		if r.Question[0].Name == testQuery {
			msg := dns.Msg{
				Answer: []dns.RR{makeRecordA("example1 3600	IN	A 10.0.0.1")},
			}
			mutex.Lock()
			answerCount1++
			mutex.Unlock()
			msg.SetReply(r)
			logErrIfNotNil(w.WriteMsg(&msg))
		}
	})

	f.addClient(NewClient(correctServer.addr, "tcp"))
	f.workerCount = 1
	req := new(dns.Msg)
	req.SetQuestion(testQuery, dns.TypeA)
	_, err := f.ServeDNS(context.TODO(), &test.ResponseWriter{}, req)
	if err != nil {
		t.FailNow()
	}
	<-time.After(time.Second)
	mutex.Lock()
	defer mutex.Unlock()
	if answerCount1 != expected {
		t.Errorf("Expected number of health checks to be %d, got : %d,", expected, answerCount1)
	}
}

func TestFanout(t *testing.T) {
	s := newServer(func(w dns.ResponseWriter, r *dns.Msg) {
		ret := new(dns.Msg)
		ret.SetReply(r)
		ret.Answer = append(ret.Answer, test.A("example.org. IN A 127.0.0.1"))
		logErrIfNotNil(w.WriteMsg(ret))
	})
	defer s.close()
	source := `fanout . %v {
	NETWORK tcp
}`
	c := caddy.NewTestController("dns", fmt.Sprintf(source, s.addr))
	f, err := parseFanout(c)
	if err != nil {
		t.Fatalf("Failed to create fanout: %s", err)
	}
	err = f.OnStartup()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer func() {
		logErrIfNotNil(f.OnShutdown())
	}()

	m := new(dns.Msg)
	m.SetQuestion("example.org.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	if _, err := f.ServeDNS(context.TODO(), rec, m); err != nil {
		t.Fatal("Expected to receive reply, but didn't")
	}
	if x := rec.Msg.Answer[0].Header().Name; x != "example.org." {
		t.Errorf("Expected %s, got %s", "example.org.", x)
	}
}
func nxdomainMsg() *dns.Msg {
	return &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeNameError},
		Question: []dns.Question{{Name: "wwww.example1.", Qclass: dns.ClassINET, Qtype: dns.TypeTXT}},
		Ns: []dns.RR{test.SOA("example1.	1800	IN	SOA	example1.net. example1.com 1461471181 14400 3600 604800 14400")},
	}
}
