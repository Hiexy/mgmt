// Mgmt
// Copyright (C) 2013-2024+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.
//
// Additional permission under GNU GPL version 3 section 7
//
// If you modify this program, or any covered work, by linking or combining it
// with embedded mcl code and modules (and that the embedded mcl code and
// modules which link with this program, contain a copy of their source code in
// the authoritative form) containing parts covered by the terms of any other
// license, the licensors of this program grant you additional permission to
// convey the resulting work. Furthermore, the licensors of this program grant
// the original author, James Shubin, additional permission to update this
// additional permission if he deems it necessary to achieve the goals of this
// additional permission.

// Package local contains functions and interfaces that are shared between
// functions and resources. It's similar to the "world" functionality, except
// that it only involves local operations that stay within a single machine or
// local mgmt instance.
package local

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/purpleidea/mgmt/util"
)

// API implements the base handle for all the methods in this package. If we
// were going to have more than one implementation for all of these, then this
// would be an interface instead, and different packages would implement it.
// Since this is not the expectation for the local API, it's all self-contained.
type API struct {
	Prefix string
	Debug  bool
	Logf   func(format string, v ...interface{})

	// Each piece of the API can take a handle here.
	*Value // TODO: Rename to ValueImpl?

	// VarDirImpl is the implementation for the VarDir API's. The API's are
	// the collection of public methods that exist on this struct.
	*VarDirImpl
}

// Init initializes the API before first use. It returns itself so it can be
// chained for API aesthetical purposes.
func (obj *API) Init() *API {
	obj.Value = &Value{}
	obj.Value.Init(&ValueInit{
		Prefix: obj.Prefix,
		Debug:  obj.Debug,
		Logf:   obj.Logf,
	})

	obj.VarDirImpl = &VarDirImpl{}
	obj.VarDirImpl.Init(&VarDirInit{
		Prefix: obj.Prefix,
		Debug:  obj.Debug,
		Logf:   obj.Logf,
	})

	return obj
}

// ValueInit are the init values that the Value API needs to work correctly.
type ValueInit struct {
	Prefix string
	Debug  bool
	Logf   func(format string, v ...interface{})
}

// Value is the API for getting, setting, and watching local values.
type Value struct {
	init         *ValueInit
	mutex        *sync.Mutex
	prefix       string
	prefixExists bool // is it okay to use the prefix?
	values       map[string]interface{}
	notify       map[chan struct{}]string // one chan (unique ptr) for each watch
	skipread     map[string]struct{}
}

// Init runs some initialization code for the Value API.
func (obj *Value) Init(init *ValueInit) {
	obj.init = init
	obj.mutex = &sync.Mutex{}
	obj.prefix = fmt.Sprintf("%s/", path.Join(obj.init.Prefix, "value"))
	obj.values = make(map[string]interface{})
	obj.notify = make(map[chan struct{}]string)
	obj.skipread = make(map[string]struct{})

	// We don't need to, or want to, load any of the keys from disk
	// initially, because (1) this would consume memory for keys we never
	// use, and (2) we can load them on first read instead.
	// TODO: build in some sort of expiry system that deletes keys older
	// than X weeks to prevent infinite growth of the on-disk database.
}

// ValueGet pulls a value out of a local in-memory, key-value store that is
// backed by on-disk storage. While each value is intended to have an underlying
// type, we use the `any` or empty `interface{}` value to represent each value
// instead of a `types.Value` because it's more generic, and not limited to
// being used with the language type system. If the value doesn't exist, we
// return a nil value and no error.
func (obj *Value) ValueGet(ctx context.Context, key string) (interface{}, error) {
	prefix, err := obj.getPrefix()
	if err != nil {
		return nil, err
	}

	obj.mutex.Lock()
	defer obj.mutex.Unlock()

	var val interface{}
	//var err error
	if _, skip := obj.skipread[key]; skip {
		val, err = valueRead(ctx, prefix, key) // must return val == nil if missing
		if err != nil {
			// We had an actual read issue. Report this and stop
			// because it means we might not be allowing our
			// cold-cache warming if we ignored it.
			return nil, err
		}
		// File not found errors are masked in the valueRead function
	}

	// Anything in memory, will override whatever we might have read.
	value, exists := obj.values[key]
	if !exists {
		// disable future disk reads since the cache is now warm!
		obj.skipread[key] = struct{}{}
		return val, nil // if val is nil, we didn't find it
	}
	return value, nil
}

// ValueSet sets a value to our in-memory, key-value store that is backed by
// on-disk storage. If you provide a nil value, this is the equivalent of
// removing or deleting the value.
func (obj *Value) ValueSet(ctx context.Context, key string, value interface{}) error {
	prefix, err := obj.getPrefix()
	if err != nil {
		return err
	}

	obj.mutex.Lock()
	defer obj.mutex.Unlock()

	// Write to state dir on disk first. If ctx cancels, we assume it's not
	// written or it doesn't matter because we're cancelling, meaning we're
	// shutting down, so our local cache can be invalidated anyways.

	if value == nil { // remove/delete
		if err := valueRemove(ctx, prefix, key); err != nil {
			return err
		}
	} else {
		if err := valueWrite(ctx, prefix, key, value); err != nil {
			return err
		}
	}

	if value == nil { // remove/delete
		delete(obj.values, key)
	} else {
		obj.values[key] = value // store to in-memory map
	}

	// We still notify on remove/delete!
	for ch, k := range obj.notify { // send notifications to any watchers...
		if k != key { // there might be more than one watcher per key
			continue
		}
		select {
		case ch <- struct{}{}: // must be async and not block forever
			// send

			// We don't ever exit here, because that would be the equivalent
			// of dropping a notification on the floor. This loop is
			// non-blocking, and so it's okay to just finish it up quickly.
			//case <-ctx.Done():
		}
	}

	return nil
}

// ValueWatch watches a value from our in-memory, key-value store that is backed
// by on-disk storage. Conveniently, it never has to watch the on-disk storage,
// because after the initial startup which always sends a single startup event,
// it suffices to watch the in-memory store for events!
func (obj *Value) ValueWatch(ctx context.Context, key string) (chan struct{}, error) {
	// No need to look at the prefix on disk, because we can do all our
	// watches from memory!
	//prefix, err := obj.getPrefix()
	//if err != nil {
	//	return nil, err
	//}

	obj.mutex.Lock()
	defer obj.mutex.Unlock()

	notifyCh := make(chan struct{}, 1) // so we can async send
	obj.notify[notifyCh] = key         // add (while within the mutex)
	notifyCh <- struct{}{}             // startup signal, send one!
	ch := make(chan struct{})
	go func() {
		defer func() { // cleanup
			obj.mutex.Lock()
			defer obj.mutex.Unlock()
			delete(obj.notify, notifyCh) // free memory (in mutex)
		}()
		for {
			select {
			case _, ok := <-notifyCh:
				if !ok {
					// programming error
					panic("unexpected channel closure")
				}
				// recv

			case <-ctx.Done():
				break // we exit
			}

			select {
			case ch <- struct{}{}:
				// send

			case <-ctx.Done():
				break // we exit
			}
		}
	}()

	return ch, nil
}

// getPrefix gets the prefix dir to use, or errors if it can't make one. It
// makes it on first use, and returns quickly from any future calls to it.
func (obj *Value) getPrefix() (string, error) {
	// NOTE: Moving this mutex to just below the first early return, would
	// be a benign race, but as it turns out, it's possible that a compiler
	// would see this behaviour as "undefined" and things might not work as
	// intended. It could perhaps be replaced with a sync/atomic primitive
	// if we wanted better performance here.
	obj.mutex.Lock()
	defer obj.mutex.Unlock()

	if obj.prefixExists { // former race read
		return obj.prefix, nil
	}

	// MkdirAll instead of Mkdir because we have no idea if the parent
	// local/ directory was already made yet or not. (If at all.) If path is
	// already a directory, MkdirAll does nothing and returns nil. (Good!)
	// TODO: I hope MkdirAll is thread-safe on path creation in case another
	// future local API tries to make the base (parent) directory too!
	if err := os.MkdirAll(obj.prefix, 0755); err != nil {
		return "", err
	}
	obj.prefixExists = true // former race write

	return obj.prefix, nil
}

func valueRead(ctx context.Context, prefix, key string) (interface{}, error) {
	// TODO: implement ctx cancellation
	// TODO: replace with my path library
	if !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("prefix is not a dir")
	}
	if !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("prefix is not absolute")
	}
	p := fmt.Sprintf("%s%s", prefix, key)

	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil // not found
	}
	if err != nil {
		return nil, err
	}
	// file exists!
	s := string(b)
	s = strings.TrimSpace(s) // get rid of any newline

	return util.B64ToValue(s)
}

func valueWrite(ctx context.Context, prefix, key string, value interface{}) error {
	// TODO: implement ctx cancellation
	// TODO: replace with my path library
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("prefix is not a dir")
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("prefix is not absolute")
	}
	p := fmt.Sprintf("%s%s", prefix, key)

	s, err := util.ValueToB64(value)
	if err != nil {
		return err
	}
	s += "\n" // files end with a newline
	return os.WriteFile(p, []byte(s), 0600)
}

func valueRemove(ctx context.Context, prefix, key string) error {
	// TODO: implement ctx cancellation
	// TODO: replace with my path library
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("prefix is not a dir")
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("prefix is not absolute")
	}
	p := fmt.Sprintf("%s%s", prefix, key)

	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil // ignore not found errors
}

// VarDirInit are the init values that the VarDir API needs to work correctly.
type VarDirInit struct {
	Prefix string
	Debug  bool
	Logf   func(format string, v ...interface{})
}

// VarDirImpl is the implementation for the VarDir API's. The API's are the
// collection of public methods that exist on this struct.
type VarDirImpl struct {
	init         *VarDirInit
	mutex        *sync.Mutex
	prefix       string
	prefixExists bool // is it okay to use the prefix?
}

// Init runs some initialization code for the VarDir API.
func (obj *VarDirImpl) Init(init *VarDirInit) {
	obj.init = init
	obj.mutex = &sync.Mutex{}
	obj.prefix = fmt.Sprintf("%s/", path.Join(obj.init.Prefix, "vardir"))
}

// VarDir returns a directory rooted at the internal prefix.
func (obj *VarDirImpl) VarDir(ctx context.Context, reldir string) (string, error) {
	if strings.HasPrefix(reldir, "/") {
		return "", fmt.Errorf("path must be relative")
	}
	if !strings.HasSuffix(reldir, "/") {
		return "", fmt.Errorf("path must be a dir")
	}
	// NOTE: The above checks ensure we don't get either "" or "/" as input!

	prefix, err := obj.getPrefix()
	if err != nil {
		return "", err
	}

	result := fmt.Sprintf("%s/", path.Join(prefix, reldir))

	// TODO: Should we mkdir this?
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	if err := os.MkdirAll(result, 0755); err != nil {
		return "", err
	}

	return result, nil
}

// getPrefix gets the prefix dir to use, or errors if it can't make one. It
// makes it on first use, and returns quickly from any future calls to it.
func (obj *VarDirImpl) getPrefix() (string, error) {
	// NOTE: Moving this mutex to just below the first early return, would
	// be a benign race, but as it turns out, it's possible that a compiler
	// would see this behaviour as "undefined" and things might not work as
	// intended. It could perhaps be replaced with a sync/atomic primitive
	// if we wanted better performance here.
	obj.mutex.Lock()
	defer obj.mutex.Unlock()

	if obj.prefixExists { // former race read
		return obj.prefix, nil
	}

	// MkdirAll instead of Mkdir because we have no idea if the parent
	// local/ directory was already made yet or not. (If at all.) If path is
	// already a directory, MkdirAll does nothing and returns nil. (Good!)
	// TODO: I hope MkdirAll is thread-safe on path creation in case another
	// future local API tries to make the base (parent) directory too!
	if err := os.MkdirAll(obj.prefix, 0755); err != nil {
		return "", err
	}
	obj.prefixExists = true // former race write

	return obj.prefix, nil
}
