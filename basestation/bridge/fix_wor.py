#!/usr/bin/env python3
"""
Watershed - disable WOR-receiver duty cycling on the E22-900T22S
(config mode required: M0=GND, M1=3.3V)

REG3 (address 0x06) bit 3 = WOR transceiver role.
Current value read earlier: 0x03 (bit3=0 = WOR receiver, duty-cycled,
"unable to transmit... works in WOR monitoring mode" per Ebyte's own
datasheet). Setting bit3=1 ("WOR transmitter" role) turns on continuous
receiving instead, per the same datasheet: "Receiving is turned on."
"""
import serial
import time

PORT = "/dev/ttyS0"
BAUD = 9600

def read_regs(ser, start, length):
    ser.reset_input_buffer()
    cmd = bytes([0xC1, start, length])
    ser.write(cmd)
    ser.flush()
    time.sleep(0.3)
    return ser.read(64)

def main():
    ser = serial.Serial(port=PORT, baudrate=BAUD, parity=serial.PARITY_NONE,
                         stopbits=serial.STOPBITS_ONE, bytesize=serial.EIGHTBITS,
                         timeout=1)

    print("--- before ---")
    resp = read_regs(ser, 0x06, 1)
    print(f"REG3 read response: {resp.hex(' ') if resp else '(none)'}")
    if not resp or len(resp) < 4:
        print("no response - is config mode active? (M0=GND, M1=3.3V)")
        ser.close()
        return

    current = resp[3]
    new_val = current | 0b00001000  # set bit 3
    print(f"REG3 current = 0x{current:02X}, writing 0x{new_val:02X}")

    write_cmd = bytes([0xC0, 0x06, 0x01, new_val])
    ser.reset_input_buffer()
    print(f"sending: {write_cmd.hex(' ')}")
    ser.write(write_cmd)
    ser.flush()
    time.sleep(0.3)
    write_resp = ser.read(64)
    print(f"write response: {write_resp.hex(' ') if write_resp else '(none)'}")

    print("--- after ---")
    resp2 = read_regs(ser, 0x06, 1)
    print(f"REG3 read response: {resp2.hex(' ') if resp2 else '(none)'}")
    if resp2 and len(resp2) >= 4:
        print(f"REG3 is now = 0x{resp2[3]:02X}")

    ser.close()

if __name__ == "__main__":
    main()
