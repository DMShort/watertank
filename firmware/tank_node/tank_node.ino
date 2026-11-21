/*
 * Watershed - tank node bench test firmware
 * -------------------------------------------
 * Stage 1 of build phase 1 ("bench-test one node on USB power"):
 * reads the JSN-SR04T level, DS18B20 temperature, and battery/rail
 * voltage, shows them on the OLED + serial monitor, and now also
 * transmits each reading over LoRa. Still no deep sleep - that gets
 * added once the base station side is confirmed receiving.
 *
 * v0.3 adds tank geometry (shape + dimensions, stored per node in NVS),
 * water height and volume in litres, a 15-byte v2 LoRa packet, a
 * median-of-5 distance read, a RADIO TX ON/OFF switch, and machine-
 * readable "@" lines for the Windows Watershed Tank Tool:
 *   @ID  {...}   reply to "ID"
 *   @CFG {...}   current config (after any change, and on "CAL SHOW")
 *   @R   {...}   one line per reading
 *   @ERR text    a command was rejected
 *
 * Board: Heltec WiFi LoRa 32 V2 (original ESP32 + SX1276).
 *
 * Sensor pin assignments match the Watershed wiring spec:
 *   I2C SDA/SCL (OLED) ....... GPIO21 / GPIO22
 *   DS18B20 data .............. GPIO4   (4.7k pull-up to 3V3)
 *   JSN-SR04T Trig ............ GPIO25
 *   JSN-SR04T Echo ............ GPIO32  (via 1k/2k divider, see Fig.5)
 *   Battery / rail sense ...... GPIO34  (ADC1, via its own divider)
 *
 * LoRa pins are fixed on this board (onboard traces to the SX1276,
 * not something you wire yourself):
 *   SCK=5  MISO=19  MOSI=27  NSS=18  RST=14  DIO0=26
 */

#include <Wire.h>
#include <SPI.h>
#include <Adafruit_GFX.h>
#include <Adafruit_SSD1306.h>
#include <OneWire.h>
#include <DallasTemperature.h>
#include <RadioLib.h>
#include <Preferences.h>

// ---- pins ----
#define PIN_I2C_SDA   21
#define PIN_I2C_SCL   22
#define PIN_DS18B20   4
#define PIN_TRIG      25
#define PIN_ECHO      32
#define PIN_VBAT      34

// ---- OLED ----
#define OLED_WIDTH    128
#define OLED_HEIGHT   64
#define OLED_ADDR     0x3C   // most 0.96" SSD1306 modules; try 0x3D if this doesn't show up
Adafruit_SSD1306 display(OLED_WIDTH, OLED_HEIGHT, &Wire, -1);

// ---- DS18B20 ----
OneWire oneWire(PIN_DS18B20);
DallasTemperature ds18b20(&oneWire);

// ---- LoRa (SX1276, onboard) ----
#define LORA_SCK   5
#define LORA_MISO  19
#define LORA_MOSI  27
#define LORA_NSS   18
#define LORA_RST   14
#define LORA_DIO0  26
const float DEFAULT_FREQ_MHZ = 868.125; // channel 18 on the Pi's E22-900T22S (850.125 + CH*1)

// best-guess match for the E22's factory-default "2.4kbps" air rate,
// back-calculated from the LoRa bitrate formula - not yet confirmed.
// now runtime-adjustable over serial for a fast sweep, no reflash needed.
const uint8_t DEFAULT_SF = 7;
const float   DEFAULT_BW = 125.0;
const uint8_t DEFAULT_CR = 7;

SPIClass loraSPI(HSPI);
Module loraModule(LORA_NSS, LORA_DIO0, LORA_RST, RADIOLIB_NC, loraSPI);
SX1276 radio(&loraModule);
bool radioOk = false;
float loraFreqMhz;
uint8_t loraSf;
float loraBw;
uint8_t loraCr;

// packet format v2 (15 bytes, little-endian, packed):
//   node_id(u8) msg_type(u8) level_pct(u8) temp_c_x10(i16) batt_mv(u16)
//   seq(u16) level_mm(u16) volume_l(u32)
// msg_type 0x02 marks this layout (v1 was 0x01, 9 bytes, no height/volume).
// sentinels: level_pct=255 / level_mm=0xFFFF / volume_l=0xFFFFFFFF = no echo,
//            temp_c_x10=-32768 = no temperature sensor
#define MSG_TYPE_READING_V2 0x02
uint16_t txSeq = 0;

#pragma pack(push, 1)
struct TankPacket {
  uint8_t  nodeId;
  uint8_t  msgType;
  uint8_t  levelPct;
  int16_t  tempCx10;
  uint16_t battMv;
  uint16_t seq;
  uint16_t levelMm;
  uint32_t volumeL;
};
#pragma pack(pop)
static_assert(sizeof(TankPacket) == 15, "TankPacket must be 15 bytes");

#define FW_VERSION "0.3"

// ---- per-node config, stored in flash (NVS) so one firmware image ----
// ---- works for every tank - set once per node over serial, no reflash ----
Preferences prefs;
uint8_t nodeId;
float calEmptyCm;   // distance reading when the tank is empty
float calFullCm;    // distance reading when the tank is full
const float DEFAULT_EMPTY_CM = 200.0;
const float DEFAULT_FULL_CM  = 20.0;
const uint8_t DEFAULT_NODE_ID = 1;

// ---- tank geometry ----
// VERT  = round tank standing upright        -> uses diameter
// HORIZ = round tank lying on its side        -> uses diameter + length
// RECT  = rectangular / slimline tank         -> uses length + width
// Height of water is measured from the "empty" calibration point, so set
// CAL EMPTY at the tank floor (or the outlet, for usable water only).
enum TankShape : uint8_t { SHAPE_VERT = 0, SHAPE_HORIZ = 1, SHAPE_RECT = 2 };
uint8_t tankShape;
float tankDiamCm;
float tankLenCm;
float tankWidthCm;
const float DEFAULT_DIAM_CM  = 300.0;
const float DEFAULT_LEN_CM   = 300.0;
const float DEFAULT_WIDTH_CM = 100.0;

bool txEnabled = true;  // RADIO TX OFF for local tests with no base station

String serialLine;

// ---- battery/rail divider ----
// TODO: set this to match whatever divider resistors end up on GPIO34.
// ratio = (R1 + R2) / R2, i.e. how much the real voltage is scaled down.
// 2.0 is a placeholder (e.g. two equal resistors) - measure with a
// multimeter against a known voltage and adjust until vbat_mv matches.
const float VBAT_DIVIDER_RATIO = 2.0;
const float ADC_REF_MV = 3300.0;
const int   ADC_MAX    = 4095;

unsigned long lastRead = 0;
const unsigned long READ_INTERVAL_MS = 2000;

float readDistanceCm() {
  digitalWrite(PIN_TRIG, LOW);
  delayMicroseconds(2);
  digitalWrite(PIN_TRIG, HIGH);
  delayMicroseconds(10);
  digitalWrite(PIN_TRIG, LOW);

  // 30ms timeout ~ 5m range, comfortably past the JSN-SR04T's spec
  unsigned long durationUs = pulseIn(PIN_ECHO, HIGH, 30000UL);
  if (durationUs == 0) {
    return -1.0; // no echo heard - out of range, or a wiring problem
  }
  return durationUs / 58.0; // standard us-to-cm conversion for this sensor family
}

// Median of n pings - one stray echo off a ladder rung or a ripple can't
// throw the reading. The JSN-SR04T needs ~60ms between pings to settle.
float readDistanceMedianCm(int n) {
  float v[9];
  if (n > 9) n = 9;
  int got = 0;
  for (int i = 0; i < n; i++) {
    float d = readDistanceCm();
    if (d > 0) v[got++] = d;
    if (i < n - 1) delay(60);
  }
  if (got == 0) return -1.0;
  // insertion sort, tiny array
  for (int i = 1; i < got; i++) {
    float k = v[i]; int j = i - 1;
    while (j >= 0 && v[j] > k) { v[j + 1] = v[j]; j--; }
    v[j + 1] = k;
  }
  return v[got / 2];
}

// highest water level the geometry allows, in cm above the empty point
float maxHeightCm() {
  float h = calEmptyCm - calFullCm;
  return h > 0 ? h : 0;
}

// water height above the empty point, clamped to 0..full; -1 = no echo
float waterHeightCm(float distanceCm) {
  if (distanceCm < 0) return -1.0;
  return constrain(calEmptyCm - distanceCm, 0.0f, maxHeightCm());
}

// litres held at a given water height for the configured shape
float volumeAtHeightL(float hCm) {
  if (hCm <= 0) return 0;
  switch (tankShape) {
    case SHAPE_HORIZ: {
      float r = tankDiamCm / 2.0;
      if (hCm > tankDiamCm) hCm = tankDiamCm;
      // circular-segment area x length
      float area = r * r * acosf((r - hCm) / r) - (r - hCm) * sqrtf(2 * r * hCm - hCm * hCm);
      return area * tankLenCm / 1000.0;
    }
    case SHAPE_RECT:
      return tankLenCm * tankWidthCm * hCm / 1000.0;
    case SHAPE_VERT:
    default: {
      float r = tankDiamCm / 2.0;
      return PI * r * r * hCm / 1000.0;
    }
  }
}

float capacityL() { return volumeAtHeightL(maxHeightCm()); }

// percent full by VOLUME (not height), so a horizontal tank reads true
float levelPctFromHeight(float hCm) {
  if (hCm < 0) return -1.0;
  float cap = capacityL();
  if (cap <= 0) return 0;
  return constrain(volumeAtHeightL(hCm) / cap * 100.0f, 0.0f, 100.0f);
}

const char* shapeName(uint8_t s) {
  switch (s) {
    case SHAPE_HORIZ: return "HORIZ";
    case SHAPE_RECT:  return "RECT";
    default:          return "VERT";
  }
}

uint16_t readBatteryMv() {
  int raw = analogRead(PIN_VBAT);
  float pinMv = (raw / (float)ADC_MAX) * ADC_REF_MV;
  return (uint16_t)(pinMv * VBAT_DIVIDER_RATIO);
}

// ---- per-node config (NVS) ----
void loadConfig() {
  prefs.begin("watershed", false);
  nodeId    = prefs.getUChar("nodeId", DEFAULT_NODE_ID);
  calEmptyCm = prefs.getFloat("emptyCm", DEFAULT_EMPTY_CM);
  calFullCm  = prefs.getFloat("fullCm", DEFAULT_FULL_CM);
  loraFreqMhz = prefs.getFloat("freqMhz", DEFAULT_FREQ_MHZ);
  loraSf = prefs.getUChar("sf", DEFAULT_SF);
  loraBw = prefs.getFloat("bw", DEFAULT_BW);
  loraCr = prefs.getUChar("cr", DEFAULT_CR);
  tankShape   = prefs.getUChar("shape", SHAPE_VERT);
  tankDiamCm  = prefs.getFloat("diamCm", DEFAULT_DIAM_CM);
  tankLenCm   = prefs.getFloat("lenCm", DEFAULT_LEN_CM);
  tankWidthCm = prefs.getFloat("widthCm", DEFAULT_WIDTH_CM);
  txEnabled   = prefs.getBool("txOn", true);
}

void printConfigJson() {
  Serial.printf("@CFG {\"node_id\":%u,\"shape\":\"%s\",\"diam_cm\":%.1f,\"len_cm\":%.1f,"
                "\"width_cm\":%.1f,\"empty_cm\":%.1f,\"full_cm\":%.1f,\"capacity_l\":%.0f,"
                "\"freq_mhz\":%.3f,\"sf\":%u,\"bw\":%.1f,\"cr\":%u,\"tx_on\":%s,\"radio_ok\":%s}\n",
                nodeId, shapeName(tankShape), tankDiamCm, tankLenCm, tankWidthCm,
                calEmptyCm, calFullCm, capacityL(), loraFreqMhz, loraSf, loraBw, loraCr,
                txEnabled ? "true" : "false", radioOk ? "true" : "false");
}

void printConfig() {
  Serial.print("config: node_id=");
  Serial.print(nodeId);
  Serial.print("  empty_cm=");
  Serial.print(calEmptyCm, 1);
  Serial.print("  full_cm=");
  Serial.print(calFullCm, 1);
  Serial.print("  freq_mhz=");
  Serial.print(loraFreqMhz, 3);
  Serial.print("  sf=");
  Serial.print(loraSf);
  Serial.print("  bw=");
  Serial.print(loraBw, 1);
  Serial.print("  cr=");
  Serial.println(loraCr);
  Serial.print("tank: shape=");
  Serial.print(shapeName(tankShape));
  Serial.print("  diam_cm=");
  Serial.print(tankDiamCm, 1);
  Serial.print("  len_cm=");
  Serial.print(tankLenCm, 1);
  Serial.print("  width_cm=");
  Serial.print(tankWidthCm, 1);
  Serial.print("  capacity_l=");
  Serial.print(capacityL(), 0);
  Serial.print("  radio_tx=");
  Serial.println(txEnabled ? "ON" : "OFF");
  printConfigJson();
}

void printHelp() {
  Serial.println("commands:");
  Serial.println("  CAL EMPTY          - capture a live reading now, save as the empty point");
  Serial.println("  CAL FULL           - capture a live reading now, save as the full point");
  Serial.println("  CAL SET EMPTY <cm> - set the empty point directly");
  Serial.println("  CAL SET FULL <cm>  - set the full point directly");
  Serial.println("  CAL SHOW           - print current config");
  Serial.println("  NODE SET <id>      - set this node's id (1-255), matches its LoRa/MQTT identity");
  Serial.println("  TANK SET SHAPE <VERT|HORIZ|RECT> - upright round / lying round / rectangular");
  Serial.println("  TANK SET DIAM <cm>   - inside diameter (VERT, HORIZ)");
  Serial.println("  TANK SET LEN <cm>    - inside length (HORIZ, RECT)");
  Serial.println("  TANK SET WIDTH <cm>  - inside width (RECT)");
  Serial.println("  RADIO TX ON|OFF      - enable/disable LoRa transmit (saved)");
  Serial.println("  ID                 - identify (used by the Windows tank tool)");
  Serial.println("  RADIO SET FREQ <mhz> - set LoRa frequency and restart");
  Serial.println("  RADIO SET SF <7-12>  - set spreading factor and restart");
  Serial.println("  RADIO SET BW <khz>   - set bandwidth (e.g. 125, 62.5, 250, 500) and restart");
  Serial.println("  RADIO SET CR <5-8>   - set coding rate denominator (4/CR) and restart");
  Serial.println("  RADIO SWEEP          - cycle through candidate SF/BW/CR combos, ~45s, sending a marker on each");
  Serial.println("  RADIO LISTEN         - reverse test: listen across the same combos, ~40s, have the Pi transmit");
  Serial.println("  HELP               - this list");
}

// non-blocking: call every loop(); dispatches a command once a full line arrives
void handleSerialCommands() {
  while (Serial.available()) {
    char c = (char)Serial.read();
    if (c == '\n' || c == '\r') {
      if (serialLine.length() == 0) continue;
      serialLine.trim();
      serialLine.toUpperCase();

      if (serialLine == "HELP") {
        printHelp();
      } else if (serialLine == "CAL SHOW") {
        printConfig();
      } else if (serialLine == "ID") {
        Serial.printf("@ID {\"fw\":\"watershed-node\",\"ver\":\"%s\",\"board\":\"heltec-v2\",\"node_id\":%u}\n",
                      FW_VERSION, nodeId);
        printConfigJson();
      } else if (serialLine.startsWith("TANK SET SHAPE ")) {
        String s = serialLine.substring(15);
        s.trim();
        if (s == "VERT")       tankShape = SHAPE_VERT;
        else if (s == "HORIZ") tankShape = SHAPE_HORIZ;
        else if (s == "RECT")  tankShape = SHAPE_RECT;
        else { Serial.println("@ERR shape must be VERT, HORIZ or RECT"); serialLine = ""; continue; }
        prefs.putUChar("shape", tankShape);
        printConfig();
      } else if (serialLine.startsWith("TANK SET DIAM ") || serialLine.startsWith("TANK SET LEN ") ||
                 serialLine.startsWith("TANK SET WIDTH ")) {
        int sp = serialLine.lastIndexOf(' ');
        float v = serialLine.substring(sp + 1).toFloat();
        if (v <= 0 || v > 5000) {
          Serial.println("@ERR dimension must be 0-5000 cm");
        } else if (serialLine.startsWith("TANK SET DIAM ")) {
          tankDiamCm = v;  prefs.putFloat("diamCm", v);  printConfig();
        } else if (serialLine.startsWith("TANK SET LEN ")) {
          tankLenCm = v;   prefs.putFloat("lenCm", v);   printConfig();
        } else {
          tankWidthCm = v; prefs.putFloat("widthCm", v); printConfig();
        }
      } else if (serialLine == "RADIO TX ON" || serialLine == "RADIO TX OFF") {
        txEnabled = (serialLine == "RADIO TX ON");
        prefs.putBool("txOn", txEnabled);
        printConfig();
      } else if (serialLine == "CAL EMPTY" || serialLine == "CAL FULL") {
        float d = readDistanceMedianCm(7);
        if (d < 0) {
          Serial.println("@ERR no echo heard - point the sensor at the target and try again");
        } else {
          if (serialLine == "CAL EMPTY") { calEmptyCm = d; prefs.putFloat("emptyCm", calEmptyCm); }
          else                           { calFullCm  = d; prefs.putFloat("fullCm", calFullCm); }
          Serial.print("captured ");
          Serial.print(d, 1);
          Serial.println(" cm, saved");
          printConfig();
        }
      } else if (serialLine.startsWith("CAL SET EMPTY ")) {
        calEmptyCm = serialLine.substring(14).toFloat();
        prefs.putFloat("emptyCm", calEmptyCm);
        printConfig();
      } else if (serialLine.startsWith("CAL SET FULL ")) {
        calFullCm = serialLine.substring(13).toFloat();
        prefs.putFloat("fullCm", calFullCm);
        printConfig();
      } else if (serialLine.startsWith("RADIO SET FREQ ")) {
        loraFreqMhz = serialLine.substring(15).toFloat();
        prefs.putFloat("freqMhz", loraFreqMhz);
        Serial.println("freq set, restarting...");
        delay(200);
        ESP.restart();
      } else if (serialLine.startsWith("RADIO SET SF ")) {
        loraSf = (uint8_t)serialLine.substring(13).toInt();
        prefs.putUChar("sf", loraSf);
        Serial.println("sf set, restarting...");
        delay(200);
        ESP.restart();
      } else if (serialLine.startsWith("RADIO SET BW ")) {
        loraBw = serialLine.substring(13).toFloat();
        prefs.putFloat("bw", loraBw);
        Serial.println("bw set, restarting...");
        delay(200);
        ESP.restart();
      } else if (serialLine.startsWith("RADIO SET CR ")) {
        loraCr = (uint8_t)serialLine.substring(13).toInt();
        prefs.putUChar("cr", loraCr);
        Serial.println("cr set, restarting...");
        delay(200);
        ESP.restart();
      } else if (serialLine == "RADIO SWEEP") {
        runRadioSweep();
      } else if (serialLine == "RADIO LISTEN") {
        runRadioListen();
      } else if (serialLine.startsWith("NODE SET ")) {
        int id = serialLine.substring(9).toInt();
        if (id < 1 || id > 255) {
          Serial.println("@ERR node id must be 1-255");
        } else {
          nodeId = (uint8_t)id;
          prefs.putUChar("nodeId", nodeId);
          printConfig();
        }
      } else {
        Serial.println("@ERR unrecognised command - try HELP");
      }

      serialLine = "";
    } else {
      serialLine += c;
    }
  }
}

// candidate (SF, BW, CR) combos to try, roughly ordered by how close they
// compute to Ebyte's "2.4kbps" default via the LoRa bitrate formula
struct SweepCombo { uint8_t sf; float bw; uint8_t cr; };
SweepCombo sweepList[] = {
  {7,  125.0, 7},
  {9,  125.0, 7},
  {7,  125.0, 5},
  {8,  125.0, 7},
  {8,  125.0, 5},
  {7,  62.5,  5},
  {7,  62.5,  7},
  {9,  125.0, 5},
  {10, 125.0, 5},
  {7,  250.0, 5},
  {8,  62.5,  5},
  {6,  125.0, 5},
};

void runRadioSweep() {
  Serial.println("=== radio sweep starting - Pi should be listening now ===");
  for (unsigned int i = 0; i < sizeof(sweepList) / sizeof(sweepList[0]); i++) {
    SweepCombo c = sweepList[i];
    int16_t st = radio.begin(loraFreqMhz, c.bw, c.sf, c.cr);
    if (st == RADIOLIB_ERR_NONE) {
      int16_t pwrState = radio.setOutputPower(20);
      Serial.print("  setOutputPower(20)=");
      Serial.println(pwrState == RADIOLIB_ERR_NONE ? "OK" : String(pwrState));
    }
    Serial.print("[");
    Serial.print(i);
    Serial.print("] sf=");
    Serial.print(c.sf);
    Serial.print(" bw=");
    Serial.print(c.bw, 1);
    Serial.print(" cr=");
    Serial.print(c.cr);
    Serial.print("  init=");
    Serial.println(st == RADIOLIB_ERR_NONE ? "OK" : String(st));
    if (st != RADIOLIB_ERR_NONE) continue;

    for (int rep = 0; rep < 3; rep++) {
      const char* marker = "WSHEDSWEEP";
      int16_t txState = radio.transmit((uint8_t*)marker, strlen(marker));
      Serial.print("  tx rep ");
      Serial.print(rep);
      Serial.print(" result=");
      Serial.println(txState == RADIOLIB_ERR_NONE ? "OK" : String(txState));
      delay(1200);
    }
  }
  Serial.println("=== sweep done - restoring saved config ===");
  radio.begin(loraFreqMhz, loraBw, loraSf, loraCr);
}

// Reverse-direction test: listen for anything at all across the same combo
// list, so the Pi's module can be the transmitter instead. Also reports
// raw RSSI/noise floor readings even when nothing decodes, since that
// alone tells us whether the antenna path is picking up anything.
void runRadioListen() {
  Serial.println("=== radio listen starting - have the Pi transmit now ===");
  for (unsigned int i = 0; i < sizeof(sweepList) / sizeof(sweepList[0]); i++) {
    SweepCombo c = sweepList[i];
    int16_t st = radio.begin(loraFreqMhz, c.bw, c.sf, c.cr);
    Serial.print("[");
    Serial.print(i);
    Serial.print("] sf=");
    Serial.print(c.sf);
    Serial.print(" bw=");
    Serial.print(c.bw, 1);
    Serial.print(" cr=");
    Serial.print(c.cr);
    Serial.print("  init=");
    Serial.println(st == RADIOLIB_ERR_NONE ? "OK" : String(st));
    if (st != RADIOLIB_ERR_NONE) continue;

    unsigned long windowEnd = millis() + 3000;
    while (millis() < windowEnd) {
      String rxStr;
      int16_t rxState = radio.receive(rxStr, 0);
      if (rxState == RADIOLIB_ERR_NONE) {
        Serial.print("  *** RECEIVED: \"");
        Serial.print(rxStr);
        Serial.print("\"  rssi=");
        Serial.print(radio.getRSSI());
        Serial.print(" snr=");
        Serial.println(radio.getSNR());
      } else if (rxState != RADIOLIB_ERR_RX_TIMEOUT) {
        Serial.print("  rx error: ");
        Serial.println(rxState);
      }
    }
    Serial.print("  ambient rssi=");
    Serial.println(radio.getRSSI());
  }
  Serial.println("=== listen done - restoring saved config ===");
  radio.begin(loraFreqMhz, loraBw, loraSf, loraCr);
}

void setup() {
  Serial.begin(115200);
  delay(300);
  Serial.println();
  Serial.println("Watershed tank node - bench test");

  loadConfig();
  printConfig();
  printHelp();

  pinMode(PIN_TRIG, OUTPUT);
  pinMode(PIN_ECHO, INPUT);
  digitalWrite(PIN_TRIG, LOW);

  analogReadResolution(12); // ADC_MAX above assumes 12-bit (0-4095)

  Wire.begin(PIN_I2C_SDA, PIN_I2C_SCL);
  if (!display.begin(SSD1306_SWITCHCAPVCC, OLED_ADDR)) {
    Serial.println("OLED not found at 0x3C - check wiring/address (try 0x3D)");
  } else {
    display.clearDisplay();
    display.setTextSize(1);
    display.setTextColor(SSD1306_WHITE);
    display.setCursor(0, 0);
    display.println("Watershed");
    display.println("bench test starting...");
    display.display();
  }

  ds18b20.begin();
  Serial.print("DS18B20 devices found: ");
  Serial.println(ds18b20.getDeviceCount());

  loraSPI.begin(LORA_SCK, LORA_MISO, LORA_MOSI, LORA_NSS);
  int16_t radioState = radio.begin(loraFreqMhz, loraBw, loraSf, loraCr);
  if (radioState == RADIOLIB_ERR_NONE) {
    radioOk = true;
    int16_t pwrState = radio.setOutputPower(20);
    Serial.print("LoRa radio init OK  freq=");
    Serial.print(loraFreqMhz, 3);
    Serial.print("  sf=");
    Serial.print(loraSf);
    Serial.print("  bw=");
    Serial.print(loraBw, 1);
    Serial.print("  setOutputPower(20)=");
    Serial.println(pwrState == RADIOLIB_ERR_NONE ? "OK" : String(pwrState));
  } else {
    radioOk = false;
    Serial.print("LoRa radio init FAILED, code ");
    Serial.println(radioState);
  }
  // announce ourselves so the Windows tool can identify the port after a reset
  Serial.printf("@ID {\"fw\":\"watershed-node\",\"ver\":\"%s\",\"board\":\"heltec-v2\",\"node_id\":%u}\n",
                FW_VERSION, nodeId);
  printConfigJson();
}

void loop() {
  handleSerialCommands();

  if (millis() - lastRead < READ_INTERVAL_MS) return;
  lastRead = millis();

  float distanceCm = readDistanceMedianCm(5);
  float heightCm = waterHeightCm(distanceCm);
  float levelPct = levelPctFromHeight(heightCm);
  float volL     = (heightCm < 0) ? -1.0 : volumeAtHeightL(heightCm);

  ds18b20.requestTemperatures();
  float tempC = ds18b20.getTempCByIndex(0);
  bool tempOk = (tempC != DEVICE_DISCONNECTED_C);

  uint16_t battMv = readBatteryMv();

  // ---- LoRa TX ----
  const char* txResult = "OFF";
  int16_t txState = 0;
  TankPacket pkt;
  pkt.nodeId   = nodeId;
  pkt.msgType  = MSG_TYPE_READING_V2;
  pkt.levelPct = (levelPct < 0) ? 255 : (uint8_t)(levelPct + 0.5f);
  pkt.tempCx10 = tempOk ? (int16_t)lroundf(tempC * 10) : -32768;
  pkt.battMv   = battMv;
  pkt.seq      = txSeq++;
  pkt.levelMm  = (heightCm < 0) ? 0xFFFF : (uint16_t)lroundf(heightCm * 10);
  pkt.volumeL  = (volL < 0) ? 0xFFFFFFFFUL : (uint32_t)lroundf(volL);

  if (!radioOk) {
    txResult = "NO_RADIO";
  } else if (txEnabled) {
    txState = radio.transmit((uint8_t*)&pkt, sizeof(pkt));
    txResult = (txState == RADIOLIB_ERR_NONE) ? "OK" : "ERR";
  }

  // ---- serial: human-readable line ----
  Serial.print("distance_cm=");
  if (distanceCm < 0) Serial.print("---"); else Serial.print(distanceCm, 1);
  Serial.print("  height_cm=");
  if (heightCm < 0) Serial.print("---"); else Serial.print(heightCm, 1);
  Serial.print("  level_pct=");
  if (levelPct < 0) Serial.print("---"); else Serial.print(levelPct, 1);
  Serial.print("  volume_l=");
  if (volL < 0) Serial.print("---"); else Serial.print(volL, 0);
  Serial.print("  temp_c=");
  if (tempOk) Serial.print(tempC, 1); else Serial.print("---");
  Serial.print("  batt_mv=");
  Serial.print(battMv);
  Serial.print("  lora_tx=");
  Serial.print(txResult);
  if (txState != 0) { Serial.print(" code="); Serial.print(txState); }
  Serial.println();

  // ---- serial: machine-readable line for the Windows tool ----
  // missing values are sent as null
  Serial.print("@R {\"seq\":");
  Serial.print(pkt.seq);
  Serial.print(",\"distance_cm\":");
  if (distanceCm < 0) Serial.print("null"); else Serial.print(distanceCm, 1);
  Serial.print(",\"height_cm\":");
  if (heightCm < 0) Serial.print("null"); else Serial.print(heightCm, 1);
  Serial.print(",\"level_pct\":");
  if (levelPct < 0) Serial.print("null"); else Serial.print(levelPct, 1);
  Serial.print(",\"volume_l\":");
  if (volL < 0) Serial.print("null"); else Serial.print(volL, 0);
  Serial.print(",\"capacity_l\":");
  Serial.print(capacityL(), 0);
  Serial.print(",\"temp_c\":");
  if (tempOk) Serial.print(tempC, 2); else Serial.print("null");
  Serial.print(",\"batt_mv\":");
  Serial.print(battMv);
  Serial.print(",\"ds18b20_count\":");
  Serial.print(ds18b20.getDeviceCount());
  Serial.print(",\"tx\":\"");
  Serial.print(txResult);
  Serial.print("\",\"uptime_s\":");
  Serial.print(millis() / 1000);
  Serial.println("}");

  // ---- OLED ----
  display.clearDisplay();
  display.setCursor(0, 0);
  display.setTextSize(1);
  display.print("Watershed  node ");
  display.println(nodeId);
  display.drawLine(0, 10, 128, 10, SSD1306_WHITE);

  display.setCursor(0, 14);
  display.print("Level: ");
  if (levelPct < 0) display.println("no echo");
  else { display.print(levelPct, 0); display.println(" %"); }

  display.setCursor(0, 25);
  display.print("Vol:   ");
  if (volL < 0) display.println("---");
  else { display.print(volL, 0); display.println(" L"); }

  display.setCursor(0, 36);
  display.print("Temp:  ");
  if (tempOk) { display.print(tempC, 1); display.println(" C"); }
  else display.println("no sensor");

  display.setCursor(0, 47);
  display.print("Supply ");
  display.print(battMv / 1000.0, 2);
  display.print("V  TX ");
  display.println(txResult);

  display.setCursor(0, 56);
  display.print("h ");
  if (heightCm < 0) display.print("---"); else display.print(heightCm, 0);
  display.print("cm  ");
  display.print(shapeName(tankShape));

  display.display();
}
