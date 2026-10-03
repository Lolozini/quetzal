// Package authkeys writes and reads the authorized_keys file a server's SFTP
// server is given: each key with the accounts, by name, it belongs to.
package authkeys

import (
	"net/url"
	"strings"

	"golang.org/x/crypto/ssh"
)

// usersOption is the authorized_keys option that names the accounts a key
// belongs to, comma separated and query-escaped.
const usersOption = "quetzal-users"

// Key is a key let in, and the accounts it belongs to. A key no account is
// named for -- in a file written before keys carried them -- is let in under
// any name.
type Key struct {
	Key   ssh.PublicKey
	Users []string
}

// Allows reports whether the key may be used to sign in as user. SFTP took any
// name with any key, so the name a session showed said nothing about whose key
// it was.
func (k Key) Allows(user string) bool {
	if len(k.Users) == 0 {
		return true
	}
	for _, u := range k.Users {
		if strings.EqualFold(u, user) {
			return true
		}
	}
	return false
}

// Line writes an authorized_keys line for a public key (in authorized_keys
// form, as an account uploads it) and the accounts it belongs to. A key that
// does not parse yields "".
func Line(publicKey string, users []string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, url.QueryEscape(u))
	}
	return usersOption + `="` + strings.Join(names, ",") + `" ` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// Parse reads an authorized_keys file, skipping lines that do not parse. A key
// listed more than once -- one key on two accounts -- gets their accounts
// together.
func Parse(data []byte) []Key {
	var out []Key
	index := map[string]int{}
	for rest := data; len(rest) > 0; {
		key, _, options, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		rest = next
		users := usersOf(options)
		blob := string(key.Marshal())
		if i, seen := index[blob]; seen {
			if len(out[i].Users) == 0 || len(users) == 0 {
				out[i].Users = nil // a key for anyone stays one
			} else {
				out[i].Users = append(out[i].Users, users...)
			}
			continue
		}
		index[blob] = len(out)
		out = append(out, Key{Key: key, Users: users})
	}
	return out
}

func usersOf(options []string) []string {
	for _, o := range options {
		v, ok := strings.CutPrefix(o, usersOption+"=")
		if !ok {
			continue
		}
		var users []string
		for _, n := range strings.Split(strings.Trim(v, `"`), ",") {
			if u, err := url.QueryUnescape(n); err == nil && u != "" {
				users = append(users, u)
			}
		}
		return users
	}
	return nil
}
