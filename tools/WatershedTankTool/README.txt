WATERSHED TANK TOOL
===================

Set up, test and update a Watershed tank node over USB. No Arduino IDE needed.

GETTING THE TOOL
Download WatershedTankTool.exe from the latest release on the project's
GitHub "Releases" page (Dave will send the link). Put it in a normal folder,
such as Desktop\Watershed, not Program Files, so it can update itself.
First run: if Windows SmartScreen says "Windows protected your PC", click
More info > Run anyway. The exe isn't code-signed.

USING IT
1. Double-click WatershedTankTool.exe. A small console window opens, plus the
   tool's own app window (it uses Edge, which is already on Windows).
2. Plug the node in by USB. The tool finds it automatically.
   - Nothing found? Try another USB cable (some are charge-only) and check
     Device Manager shows a "Silicon Labs CP210x" COM port. If not, install
     the CP210x driver from silabs.com.
3. Firmware & updates box: if it says "Update available", click
   "Install firmware". Keep the cable plugged in until it says Installed.
   The tank settings and calibration on the node are kept.
   - If it says the board didn't enter download mode: hold the PRG button
     on the board, click Install again, and let go once it says Connecting.
4. Tank setup: pick the shape, enter the INSIDE dimensions in cm, and the
   empty and full distances measured from the sensor face. Click "Save to node".
5. Local test at the tank: choose a duration, optionally a dipstick depth,
   then Start. Results are saved in the "logs" folder next to the exe. Send
   the _summary.txt file to Dave.

UPDATES
The tool checks GitHub when it starts and every 6 hours. A newer tool
installs itself and the window reloads. Newer node firmware shows an
"Install firmware" button. "Check now" checks straight away.

Closing the app window quits the tool after about 20 seconds (or click Quit).

FOR DAVE
Source is in src\ (Go, standard library only). Releases are built by
.github\workflows\release.yml. See RELEASING.md at the top of the repo.
The exe built locally is a development build: no update source, so no
automatic updates. Dad should use the release exe.
