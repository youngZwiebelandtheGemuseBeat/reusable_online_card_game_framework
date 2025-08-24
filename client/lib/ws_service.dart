import 'dart:async';
import 'dart:convert';
import 'dart:math' as math;
import 'dart:html' as html;

class WsService {
  final _messages = StreamController<Map<String, dynamic>>.broadcast();
  Stream<Map<String, dynamic>> get messages => _messages.stream;

  html.WebSocket? _ws;
  bool _isOpen = false;
  bool _manuallyClosed = false;
  final _outbox = <String>[];

  Timer? _reconnectTimer;
  int _retryAttempts = 0;

  Timer? _pingTimer;

  /// Public API
  void connect({String? explicitUrl}) {
    _manuallyClosed = false;
    _openSocket(explicitUrl: explicitUrl);
  }

  void close() {
    _manuallyClosed = true;
    _pingTimer?.cancel();
    _pingTimer = null;
    _reconnectTimer?.cancel();
    _reconnectTimer = null;
    try {
      _ws?.close();
    } catch (_) {}
    _ws = null;
    _isOpen = false;
  }

  void send(Map<String, dynamic> msg) {
    final encoded = jsonEncode(msg);
    if (_isOpen && _ws != null && _ws!.readyState == html.WebSocket.OPEN) {
      try {
        _ws!.sendString(encoded);
      } on Object {
        // Safari can still race throws; enqueue fallback
        _outbox.add(encoded);
      }
    } else {
      // Queue until OPEN (Safari throws InvalidStateError if we send too early)
      _outbox.add(encoded);
    }
  }

  // ---- internals ----

  String _deriveUrl({String? explicitUrl}) {
    if (explicitUrl != null && explicitUrl.isNotEmpty) return explicitUrl;

    final loc = html.window.location;
    final scheme = loc.protocol == 'https:' ? 'wss' : 'ws';
    final host = loc.hostname;
    final port = (loc.port != null && loc.port!.isNotEmpty) ? ':${loc.port}' : '';
    // default endpoint; change if your server uses a different path
    const path = '/ws';
    return '$scheme://$host$port$path';
  }

  void _openSocket({String? explicitUrl}) {
    final url = _deriveUrl(explicitUrl: explicitUrl);
    // Ensure any prior timers are cleared
    _reconnectTimer?.cancel();
    _reconnectTimer = null;

    // Some WebKit builds are picky about protocols array vs single string;
    // single-string constructor keeps things simple.
    html.WebSocket ws;
    try {
      ws = html.WebSocket(url);
      // Disable binaryType fiddling for Safari; keep default 'blob'
    } on Object {
      _scheduleReconnect();
      return;
    }

    _ws = ws;
    _isOpen = false;

    ws.onOpen.listen((_) {
      _isOpen = true;
      _retryAttempts = 0;

      // Flush outbox safely
      if (_outbox.isNotEmpty) {
        final copy = List<String>.from(_outbox);
        _outbox.clear();
        for (final s in copy) {
          try {
            ws.sendString(s);
          } catch (_) {
            _outbox.add(s); // keep if send fails
            break;
          }
        }
      }

      // Start lightweight app-level ping to keep Safari connections warm
      _pingTimer?.cancel();
      _pingTimer = Timer.periodic(const Duration(seconds: 25), (_) {
        // Keepalive only if still open
        if (_isOpen && _ws != null && _ws!.readyState == html.WebSocket.OPEN) {
          try {
            ws.sendString(jsonEncode({"t": "ping"}));
          } catch (_) {
            // ignore; reconnect path will handle it
          }
        }
      });
    });

    ws.onMessage.listen((evt) {
      try {
        final data = evt.data;
        if (data is String) {
          final decoded = jsonDecode(data);
          if (decoded is Map<String, dynamic>) {
            _messages.add(decoded);
          } else if (decoded is Map) {
            _messages.add(decoded.map((k, v) => MapEntry(k.toString(), v)));
          }
        }
      } catch (_) {
        // ignore malformed frames
      }
    });

    ws.onError.listen((_) {
      // let close handler own the reconnection
    });

    ws.onClose.listen((_) {
      _isOpen = false;
      _pingTimer?.cancel();
      _pingTimer = null;
      _ws = null;
      if (!_manuallyClosed) {
        _scheduleReconnect();
      }
    });

    // Handle tab visibility changes (Safari throttling)
    html.document.addEventListener('visibilitychange', (_) {
      final vis = html.document.visibilityState;
      if (vis == 'visible') {
        // Nudge connection if it died while backgrounded
        if (!_isOpen && !_manuallyClosed && _reconnectTimer == null) {
          _scheduleReconnect(immediate: true);
        }
      }
    });
  }

  void _scheduleReconnect({bool immediate = false}) {
    if (_manuallyClosed) return;
    _reconnectTimer?.cancel();

    // Exponential backoff with jitter (max ~10s)
    final base = math.min(10, 1 << math.min(6, _retryAttempts)); // 1,2,4,8,10,10...
    final jitterMs = math.Random().nextInt(400); // add up to 400ms
    final delay = immediate ? Duration.zero : Duration(seconds: base, milliseconds: jitterMs);
    _reconnectTimer = Timer(delay, () {
      _retryAttempts++;
      _openSocket();
    });
  }
}
