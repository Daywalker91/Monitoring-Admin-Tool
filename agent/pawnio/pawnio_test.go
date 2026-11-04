package pawnio

import "testing"

func TestIntelTemp(t *testing.T) {
	// i7-3770: TjMax 105 °C (0x69 in Bits 23:16), Abstand 60 °C → 45 °C.
	tj := tjMax(0x0069_0000)
	if tj != 105 {
		t.Fatalf("tjmax %v", tj)
	}
	if c, ok := intelTemp(tj, 0x003C_0000, false); !ok || c != 45 {
		t.Fatalf("package %v %v", c, ok)
	}
	// Kern-Register: ohne "Reading Valid" (Bit 31) kein Wert.
	if _, ok := intelTemp(tj, 0x003C_0000, true); ok {
		t.Fatal("ungültiger kernwert akzeptiert")
	}
	if c, ok := intelTemp(tj, 0x8032_0000, true); !ok || c != 55 {
		t.Fatalf("kern %v %v", c, ok)
	}
	if tjMax(0) != 100 {
		t.Fatal("tjmax 0 → 100")
	}
}

func TestAMDTemp(t *testing.T) {
	// 50 °C = 400 Schritte zu 0,125 °C in Bits 31:21.
	if c := amdTemp(400<<21, 0); c != 50 {
		t.Fatalf("normal %v", c)
	}
	// Erweiterter Bereich (Bit 19 bzw. Bits 17:16 = 11): 49 °C abziehen.
	if c := amdTemp(400<<21|1<<19, 0); c != 1 {
		t.Fatalf("bit 19 %v", c)
	}
	if c := amdTemp(800<<21|0x30000, 0); c != 51 {
		t.Fatalf("tj_sel %v", c)
	}
	if c := amdTemp(560<<21, amdOffset("AMD Ryzen 7 1800X Eight-Core Processor")); c != 50 {
		t.Fatalf("1800X %v", c)
	}
	for name, want := range map[string]float64{
		"AMD Ryzen 5 3600XT 6-Core Processor":            0,
		"AMD Ryzen 7 2700X Eight-Core Processor":         -10,
		"AMD Ryzen Threadripper 2950X 16-Core Processor": -27,
	} {
		if got := amdOffset(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestExecInput(t *testing.T) {
	in := execInput("ioctl_read_msr", 0x1A2)
	if len(in) != 40 || string(in[:14]) != "ioctl_read_msr" || in[14] != 0 || in[31] != 0 {
		t.Fatalf("name: %q", in[:32])
	}
	if le64(in[32:]) != 0x1A2 || in[32] != 0xA2 || in[33] != 0x01 {
		t.Fatalf("argument: % x", in[32:])
	}
	// Zu lange Namen werden gekürzt, das letzte Byte bleibt NUL.
	if in := execInput("ein_sehr_langer_funktionsname_ueber_32", 1); in[31] != 0 {
		t.Fatal("name ohne nul")
	}
}
