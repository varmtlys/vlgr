package server

import "encoding/binary"

// extractSNI parses the server_name from a buffered TLS ClientHello record.
// It reads only what the handshake bytes describe and never mutates them, so
// the same bytes can be forwarded verbatim (true passthrough). Returns false
// when data is not a complete ClientHello or carries no SNI.
func extractSNI(data []byte) (string, bool) {
	// TLS record: type(1)=0x16 handshake, version(2), length(2), fragment.
	if len(data) < 5 || data[0] != 0x16 {
		return "", false
	}
	recLen := int(binary.BigEndian.Uint16(data[3:5]))
	hs := data[5:]
	if len(hs) < recLen {
		return "", false // record not fully buffered yet
	}
	hs = hs[:recLen]

	// Handshake: type(1)=0x01 ClientHello, length(3), body.
	if len(hs) < 4 || hs[0] != 0x01 {
		return "", false
	}
	b := hs[4:]

	// client_version(2) + random(32).
	if len(b) < 34 {
		return "", false
	}
	b = b[34:]

	// session_id.
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// cipher_suites (2-byte length).
	if len(b) < 2 {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < n {
		return "", false
	}
	b = b[n:]

	// compression_methods (1-byte length).
	if len(b) < 1 || len(b) < 1+int(b[0]) {
		return "", false
	}
	b = b[1+int(b[0]):]

	// extensions (2-byte length).
	if len(b) < 2 {
		return "", false
	}
	extTotal := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < extTotal {
		return "", false
	}
	b = b[:extTotal]

	for len(b) >= 4 {
		extType := binary.BigEndian.Uint16(b)
		extLen := int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		if len(b) < extLen {
			return "", false
		}
		if extType != 0x0000 { // server_name
			b = b[extLen:]
			continue
		}
		return parseServerName(b[:extLen])
	}
	return "", false
}

// parseServerName reads the first host_name entry from a server_name extension.
func parseServerName(ext []byte) (string, bool) {
	// server_name_list length(2), then entries: type(1), name length(2), name.
	if len(ext) < 2 {
		return "", false
	}
	listLen := int(binary.BigEndian.Uint16(ext))
	ext = ext[2:]
	if len(ext) < listLen {
		return "", false
	}
	for len(ext) >= 3 {
		nameType := ext[0]
		nameLen := int(binary.BigEndian.Uint16(ext[1:]))
		ext = ext[3:]
		if len(ext) < nameLen {
			return "", false
		}
		if nameType == 0x00 { // host_name
			return string(ext[:nameLen]), true
		}
		ext = ext[nameLen:]
	}
	return "", false
}
