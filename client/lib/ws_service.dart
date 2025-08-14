import 'dart:async';
import 'dart:convert';
import 'dart:html' as html; // web-only
import 'package:web_socket_channel/web_socket_channel.dart';

String _deriveWsUrl() {
  const env = String.fromEnvironment('WS_URL', defaultValue: '');
  if (env.isNotEmpty) return env;
  final loc = html.window.location;
  final scheme = (loc.protocol == 'https:') ? 'wss' : 'ws';
  final host = loc.host; // includes :port
  return '$scheme://$host/ws';
}

class WsService {
  WsService();

  late final Uri url = Uri.parse(_deriveWsUrl());
  WebSocketChannel? _ch;
  final _inCtrl = StreamController<Map<String, dynamic>>.broadcast();
  Stream<Map<String, dynamic>> get messages => _inCtrl.stream;

  Future<void> connect() async {
    _ch = WebSocketChannel.connect(url);
    _ch!.stream.listen((data) {
      try {
        _inCtrl.add(jsonDecode(data as String));
      } catch (_) {}
    }, onDone: _reconnect, onError: (_) => _reconnect());
    send({"t": "join", "m": {}});
  }

  void send(Map<String, dynamic> msg) {
    _ch?.sink.add(jsonEncode(msg));
  }

  void _reconnect() {
    Future.delayed(const Duration(seconds: 2), connect);
  }
}
