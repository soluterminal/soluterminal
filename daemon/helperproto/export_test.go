package helperproto

import "net"

// OpenRawForTest opens a stream and writes line as its header unchecked, so
// tests can send headers the typed Open would never produce.
func (s *HelperSession) OpenRawForTest(line []byte) (net.Conn, error) {
	return openRaw(s.mux.Open, line)
}

// OpenRawForTest is HelperSession.OpenRawForTest from the host end.
func (s *HostSession) OpenRawForTest(line []byte) (net.Conn, error) {
	return openRaw(s.mux.Open, line)
}

func openRaw[C net.Conn](open func() (C, error), line []byte) (net.Conn, error) {
	st, err := open()
	if err != nil {
		return nil, err
	}
	if _, err := st.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}
