package publicweb

import (
	"errors"
	"net"
	"testing"
)

func TestPublicOnly(t *testing.T) {
	cases := []struct {
		addr string
		want bool // true: allowed
	}{
		{"93.184.216.34:443", true},
		{"[2606:4700:4700::1111]:443", true},
		{"8.8.8.8:443", true},
		{"127.0.0.1:443", false},
		{"10.1.2.3:443", false},
		{"172.16.0.1:443", false},
		{"192.168.1.1:443", false},
		{"169.254.169.254:80", false},
		{"0.0.0.0:80", false},
		{"0.1.2.3:80", false},
		{"100.64.0.1:443", false},
		{"100.127.255.254:443", false},
		{"198.18.0.1:443", false},
		{"198.19.255.1:443", false},
		{"192.0.0.8:443", false},
		{"192.0.2.10:443", false},
		{"198.51.100.7:443", false},
		{"203.0.113.9:443", false},
		{"240.0.0.1:443", false},
		{"255.255.255.255:443", false},
		{"224.0.0.1:443", false},
		{"[::]:443", false},
		{"[::1]:443", false},
		{"[fe80::1]:443", false},
		{"[fc00::1]:443", false},
		{"[fd12:3456::1]:443", false},
		{"[64:ff9b::a00:1]:443", false},
		{"[64:ff9b:1::1]:443", false},
		{"[100::1]:443", false},
		{"[2001:db8::1]:443", false},
		{"[::ffff:10.0.0.1]:443", false},
		{"[::ffff:100.64.0.1]:443", false},
		{"not-an-ip:443", false},
	}
	for _, c := range cases {
		err := PublicOnly("tcp", c.addr, nil)
		if (err == nil) != c.want {
			t.Errorf("%s: err %v", c.addr, err)
		}
		if err != nil && !errors.Is(err, ErrNotPublic) {
			t.Errorf("%s: want ErrNotPublic, got %v", c.addr, err)
		}
	}
	if IsPublic(net.IP(nil)) {
		t.Fatal("nil is not public")
	}
}
