package update

// ReleaseKey is the minisign public key every release's sha256sums.txt.minisig must
// verify under (docs/12 §4, I-128). The private half lives only in the CI secret
// MINISIGN_SECRET_KEY and in the maintainer's key file (`tailraid-sign -gen`). An empty
// key makes `update` refuse every release; a new key pair replaces this constant and the
// secret together, and every node updates once more under the old key first.
const ReleaseKey = `untrusted comment: tailraid release signing key
RWQZh3kca5j1N8FXgFtm3kxOE8RBttUOd0MlVyZcg3Va4XXCkDWxw+E+
`
