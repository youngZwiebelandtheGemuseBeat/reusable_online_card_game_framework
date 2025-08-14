import 'dart:async';
import 'dart:convert';
import 'package:web_socket_channel/web_socket_channel.dart';

class WsService {
  final Uri url;
  WebSocketChannel? _ch;
  final _inCtrl = StreamController<Map<String, dynamic>>.broadcast();
  Stream<Map<String, dynamic>> get messages => _inCtrl.stream;

  WsService(String urlStr) : url = Uri.parse(urlStr);

  Future<void> connect() async {
    _ch = WebSocketChannel.connect(url);
    _ch!.stream.listen((data) {
      try { _inCtrl.add(jsonDecode(data as String)); } catch (_) {}
    }, onDone: _reconnect, onError: (_) => _reconnect());
    send({"t":"join","m":{}});
  }

  void send(Map<String, dynamic> msg) {
    _ch?.sink.add(jsonEncode(msg));
  }

  void _reconnect() {
    Future.delayed(const Duration(seconds: 2), () => connect());
  }
}