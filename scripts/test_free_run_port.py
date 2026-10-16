import contextlib
import io
import shutil
import socket
import subprocess
import sys
import unittest

from free_run_port import listener_pids, stop_listeners


@unittest.skipUnless(shutil.which("lsof") or shutil.which("ss"), "requires lsof or ss")
class FreeRunPortTests(unittest.TestCase):
    def listener(self, ignore_term=False):
        child = subprocess.Popen(
            [sys.executable, "-c", """
import signal, socket, sys, time
if sys.argv[1] == 'True':
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
s = socket.socket()
s.bind(('127.0.0.1', 0))
s.listen()
print(s.getsockname()[1], flush=True)
while True:
    time.sleep(1)
""", str(ignore_term)],
            stdout=subprocess.PIPE, text=True,
        )
        self.addCleanup(child.stdout.close)
        self.addCleanup(lambda: child.wait(timeout=3))
        self.addCleanup(lambda: child.kill() if child.poll() is None else None)
        return child, int(child.stdout.readline())

    def test_unused_port_does_not_stop_other_listener(self):
        child, occupied = self.listener()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            free = sock.getsockname()[1]
        stop_listeners(free)
        self.assertIsNone(child.poll())
        self.assertIn(child.pid, listener_pids(occupied))

    def test_listener_stops_gracefully(self):
        child, port = self.listener()
        with contextlib.redirect_stdout(io.StringIO()):
            stop_listeners(port)
        self.assertEqual(child.wait(timeout=3), -15)
        self.assertEqual(listener_pids(port), set())

    def test_unresponsive_listener_is_forced(self):
        child, port = self.listener(ignore_term=True)
        with contextlib.redirect_stdout(io.StringIO()):
            stop_listeners(port, timeout=0.1)
        self.assertEqual(child.wait(timeout=3), -9)
        self.assertEqual(listener_pids(port), set())


if __name__ == "__main__":
    unittest.main()
