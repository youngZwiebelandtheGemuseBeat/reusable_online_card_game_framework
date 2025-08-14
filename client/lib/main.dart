import 'package:flutter/material.dart';
import 'ws_service.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const AppRoot());
}

class AppRoot extends StatefulWidget {
  const AppRoot({super.key});
  @override
  State<AppRoot> createState() => _AppRootState();
}

class _AppRootState extends State<AppRoot> {
  final ws = WsService();

  @override
  void initState() {
    super.initState();
    ws.connect();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Lobby',
      theme: ThemeData(useMaterial3: true, colorSchemeSeed: Colors.blueGrey),
      home: LobbyPage(ws: ws),
    );
  }
}

// -------------------- Lobby --------------------

class LobbyPage extends StatefulWidget {
  final WsService ws;
  const LobbyPage({super.key, required this.ws});
  @override
  State<LobbyPage> createState() => _LobbyPageState();
}

class _LobbyPageState extends State<LobbyPage> {
  List<Map<String, dynamic>> rooms = [];
  String? createdRoomId;
  final roomCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    widget.ws.messages.listen((m) {
      if (!mounted) return;
      if (m['t'] == 'rooms') {
        final list = (m['m']['list'] as List).cast<Map>().map((e) {
          return e.map((k, v) => MapEntry(k.toString(), v));
        }).toList();
        setState(() => rooms = List<Map<String, dynamic>>.from(list));
      } else if (m['t'] == 'created') {
        setState(() {
          createdRoomId = m['m']['room'] as String?;
          roomCtrl.text = createdRoomId ?? '';
        });
      }
    });
    // request initial snapshot
    widget.ws.send({"t":"list_rooms"});
  }

  @override
  void dispose() {
    roomCtrl.dispose();
    super.dispose();
  }

  void _create() {
    widget.ws.send({"t":"create_table","m":{"game":"mulatschak","seats":3}});
  }

  void _joinById(String id) {
    if (id.isEmpty) return;
    widget.ws.send({"t":"join_table","m":{"room":id}});
    // Navigate to table page; it will render when 'state' arrives.
    Navigator.of(context).push(MaterialPageRoute(
      builder: (_) => TablePage(ws: widget.ws, roomId: id),
    ));
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Lobby')),
      body: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Wrap(spacing: 8, runSpacing: 8, children: [
              FilledButton(onPressed: _create, child: const Text('Create table (3)')),
              SizedBox(
                width: 280,
                child: TextField(
                  controller: roomCtrl,
                  decoration: const InputDecoration(
                    labelText: 'Room ID',
                    hintText: 'Paste to join',
                    isDense: true,
                  ),
                ),
              ),
              OutlinedButton(
                onPressed: () => _joinById(roomCtrl.text.trim()),
                child: const Text('Join by ID'),
              ),
            ]),
            const SizedBox(height: 16),
            const Text('Open tables:', style: TextStyle(fontWeight: FontWeight.bold)),
            const SizedBox(height: 8),
            Expanded(
              child: rooms.isEmpty
                  ? const Center(child: Text('No tables yet. Create one!'))
                  : ListView.separated(
                      itemCount: rooms.length,
                      separatorBuilder: (_, __) => const Divider(),
                      itemBuilder: (_, i) {
                        final r = rooms[i];
                        final id = (r['id'] ?? '').toString();
                        final seats = (r['seats'] as num).toInt();
                        final occ = (r['occupied'] as num).toInt();
                        final started = (r['started'] ?? false) as bool;
                        final missing = seats - occ;
                        final full = occ >= seats;
                        return ListTile(
                          title: Text('Room $id'),
                          subtitle: Text(
                            started
                              ? 'In play • $occ/$seats seated'
                              : (full ? 'Full • $occ/$seats' : 'Waiting • $occ/$seats • ${missing} missing'),
                          ),
                          trailing: FilledButton.tonalIcon(
                            onPressed: full ? null : () => _joinById(id),
                            icon: const Icon(Icons.play_arrow),
                            label: Text(full ? 'Full' : 'Join'),
                          ),
                        );
                      },
                    ),
            ),
          ],
        ),
      ),
    );
  }
}

// -------------------- Table page --------------------

class TablePage extends StatefulWidget {
  final WsService ws;
  final String roomId;
  const TablePage({super.key, required this.ws, required this.roomId});

  @override
  State<TablePage> createState() => _TablePageState();
}

class _TablePageState extends State<TablePage> {
  int? seat, turn;
  String? trump;
  List<dynamic> hand = [];
  final chat = <String>[];
  final chatCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    widget.ws.messages.listen((m) {
      if (!mounted) return;
      switch (m['t']) {
        case 'state':
          if ((m['m']['room'] ?? '') == widget.roomId) {
            setState(() {
              seat = (m['m']['seat'] as num).toInt();
              turn = (m['m']['turn'] as num).toInt();
              trump = m['m']['trump'] as String?;
              hand = List<dynamic>.from(m['m']['you'] as List? ?? const []);
            });
          }
          break;
        case 'chat':
          if ((m['m']['room'] ?? '') == widget.roomId) {
            final from = (m['m']['from'] ?? 'player').toString();
            final text = (m['m']['text'] ?? '').toString();
            setState(() => chat.add('[$from] $text'));
          }
          break;
      }
    });
    // If we arrived here without having joined (deep link), try join now:
    widget.ws.send({"t":"join_table","m":{"room": widget.roomId}});
  }

  @override
  void dispose() {
    chatCtrl.dispose();
    super.dispose();
  }

  void _sendChat() {
    final t = chatCtrl.text.trim();
    if (t.isEmpty) return;
    widget.ws.send({"t":"chat","m":{"room": widget.roomId, "text": t}});
    chatCtrl.clear();
  }

  void _leave() {
    widget.ws.send({"t":"leave_table","m":{"room": widget.roomId}});
    Navigator.of(context).pop(); // back to Lobby
  }

  @override
  Widget build(BuildContext context) {
    final myTurn = seat != null && turn != null && seat == turn;
    return Scaffold(
      appBar: AppBar(
        title: Text('Table — ${widget.roomId}'),
        leading: IconButton(icon: const Icon(Icons.arrow_back), onPressed: _leave),
      ),
      body: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Seat: ${seat ?? "-"}  |  Turn: ${turn ?? "-"}  |  Trump: ${trump ?? "-"}'),
            const Divider(),
            const Text('Your hand:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: hand.map<Widget>((c) {
                final suit = c['Suit'] as String? ?? '?';
                final rank = c['Rank'] as String? ?? '?';
                return OutlinedButton(
                  onPressed: myTurn ? () {
                    widget.ws.send({
                      "t":"move","m":{
                        "room": widget.roomId, "seat": seat,
                        "type":"play_card",
                        "card":{"Suit": suit, "Rank": rank}
                      }
                    });
                  } : null,
                  child: Text('$rank-$suit'),
                );
              }).toList(),
            ),
            const Divider(),
            const Text('Chat:'),
            Row(
              children: [
                Expanded(
                  child: TextField(
                    controller: chatCtrl,
                    decoration: const InputDecoration(hintText: 'Type a message…', isDense: true),
                    onSubmitted: (_) => _sendChat(),
                  ),
                ),
                const SizedBox(width: 8),
                FilledButton(onPressed: _sendChat, child: const Text('Send')),
              ],
            ),
            const SizedBox(height: 8),
            Expanded(
              child: Container(
                decoration: BoxDecoration(
                  border: Border.all(color: Colors.black12),
                  borderRadius: BorderRadius.circular(6),
                ),
                padding: const EdgeInsets.all(8),
                child: ListView.builder(
                  itemCount: chat.length,
                  itemBuilder: (_, i) => Text(chat[i]),
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
