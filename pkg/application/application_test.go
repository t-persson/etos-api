// Copyright Axis Communications AB.
//
// For a full list of individual contributors, please see the commit history.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package application

import (
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/assert"
)

type testApp struct {
	route   string
	message string
}

// testRoute is a test route that prints a test message from the app to which it is "attached".
func (t *testApp) testRoute(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	if _, err := w.Write([]byte(t.message)); err != nil {
		log.Printf("failed to write test response: %v", err)
	}
}

// LoadRoutes from the application to which it is "attached".
func (t *testApp) LoadRoutes(router *httprouter.Router) {
	router.GET(t.route, t.testRoute)
}

// Close is a placeholder to fulfill implementation of the application interface.
func (t *testApp) Close() {}

// TestNew verifies that it is possible to load a handlers routes.
func TestNew(t *testing.T) {
	app := &testApp{
		route:   "/testing",
		message: "hello",
	}
	router := New(app)

	responseRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", app.route, nil)
	router.ServeHTTP(responseRecorder, request)
	assert.Equal(t, 200, responseRecorder.Code)
	assert.Equal(t, app.message, responseRecorder.Body.String())
}

// TestNew verifies that it is possible to load multiple handlers routes.
func TestNewMultiple(t *testing.T) {
	route1 := &testApp{"/route1", "hello1"}
	route2 := &testApp{"/route2", "hello2"}
	tests := []struct {
		name string
		app  *testApp
	}{
		{name: "Route1", app: route1},
		{name: "Route1", app: route2},
	}

	router := New(route1, route2)

	for _, testCase := range tests {
		responseRecorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", testCase.app.route, nil)
		router.ServeHTTP(responseRecorder, request)
		assert.Equal(t, 200, responseRecorder.Code)
		assert.Equal(t, testCase.app.message, responseRecorder.Body.String())
	}
}
