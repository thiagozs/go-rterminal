package relay

// MultiTap fans a session's traffic out to several taps, skipping nil ones.
// It returns nil when there is nothing to tap, so the relay avoids the
// per-message work.
func MultiTap(taps ...Tap) Tap {
	var list multiTap
	for _, t := range taps {
		if t != nil {
			list = append(list, t)
		}
	}
	switch len(list) {
	case 0:
		return nil
	case 1:
		return list[0]
	}
	return list
}

type multiTap []Tap

func (m multiTap) Output(data []byte) {
	for _, t := range m {
		t.Output(data)
	}
}

func (m multiTap) Resize(cols, rows int) {
	for _, t := range m {
		t.Resize(cols, rows)
	}
}

func (m multiTap) Close() {
	for _, t := range m {
		t.Close()
	}
}
