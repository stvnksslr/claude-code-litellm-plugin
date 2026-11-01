module github.com/stvnksslr/claude-code-litellm-plugin

go 1.27.3

// Keep pre-1.27 runtime defaults, notably x509sslcertoverrideplatform=0: on
// macOS/Windows, SSL_CERT_FILE/SSL_CERT_DIR must not override the system
// Keychain / cert store (corporate roots, MDM CAs).
godebug default=go1.26
