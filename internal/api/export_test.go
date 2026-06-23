package api

// HashTokenForTest exposes the internal token hash to external (_test) packages,
// so a test can seed a password-reset row with a known token.
var HashTokenForTest = hashToken
