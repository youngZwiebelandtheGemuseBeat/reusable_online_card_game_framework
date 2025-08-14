import 'dart:io' show Platform;
import 'package:flutter/material.dart';
import 'ws_service.dart';

const wsUrl = String.fromEnvironment('WS_URL', defaultValue: 'ws://localhost:8080/ws');

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const CardApp());
}

class CardApp extends StatefulWidget { const CardApp({super.key}); @override State<CardApp> createState() => _CardAppState(); }

class _CardAppState extends State<CardApp> {
  final ws = WsService(wsUrl);
  final log = <String>[];

  @override
  void initState() {
    super.initState();
    ws.connect();
    ws.messages.listen((m) { setState(() { log.add(m.toString()); }); });
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      home: Scaffold(
        appBar: AppBar(title: const Text('CardPlatform — Mulatschak v1')),
        body: Column(children: [
          Expanded(child: ListView.builder(
            itemCount: log.length,
            itemBuilder: (_, i) => Padding(
              padding: const EdgeInsets.all(8), child: Text(log[i]),
            ),
          )),
          Padding(
            padding: const EdgeInsets.all(12),
            child: Row(children: [
              ElevatedButton(onPressed: () => ws.send({"t":"chat","m":{"text":"hi"}}), child: const Text('Chat: hi')),
            ]),
          )
        ]),
      ),
    );
  }
}