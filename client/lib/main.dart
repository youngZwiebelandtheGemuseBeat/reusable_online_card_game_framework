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
  final nameCtrl = TextEditingController(text: '');

  @override
  void initState() {
    super.initState();
    ws.connect();
  }

  @override
  void dispose() {
    nameCtrl.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Lobby',
      theme: ThemeData(useMaterial3: true, colorSchemeSeed: Colors.blueGrey),
      home: Scaffold(
        appBar: AppBar(title: const Text('Lobby')),
        body: LobbyPage(ws: ws, nameCtrl: nameCtrl),
      ),
    );
  }
}

// -------------------- Lobby --------------------

class LobbyPage extends StatefulWidget {
  final WsService ws;
  final TextEditingController nameCtrl;
  const LobbyPage({super.key, required this.ws, required this.nameCtrl});
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
    widget.ws.send({"t":"list_rooms"});
  }

  @override
  void dispose() { roomCtrl.dispose(); super.dispose(); }

  void _applyName() {
    final n = widget.nameCtrl.text.trim();
    if (n.isNotEmpty) widget.ws.send({"t":"set_name","m":{"name": n}});
  }

  void _create() {
    _applyName();
    widget.ws.send({"t":"create_table","m":{"game":"mulatschak","seats":3}});
  }

  void _joinById(String id) {
    if (id.isEmpty) return;
    _applyName();
    widget.ws.send({"t":"join_table","m":{"room":id}});
    Navigator.of(context).push(MaterialPageRoute(
      builder: (_) => TablePage(ws: widget.ws, roomId: id),
    ));
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.all(12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          // Name field
          Row(children: [
            SizedBox(
              width: 260,
              child: TextField(
                controller: widget.nameCtrl,
                decoration: const InputDecoration(
                  labelText: 'Your name (optional)', isDense: true,
                ),
                onSubmitted: (_) => _applyName(),
              ),
            ),
            const SizedBox(width: 8),
            OutlinedButton(onPressed: _applyName, child: const Text('Set name')),
          ]),
          const SizedBox(height: 12),

          Wrap(spacing: 8, runSpacing: 8, children: [
            FilledButton(onPressed: _create, child: const Text('Create table (3)')),
            SizedBox(
              width: 280,
              child: TextField(
                controller: roomCtrl,
                decoration: const InputDecoration(
                  labelText: 'Room ID', hintText: 'Paste to join', isDense: true,
                ),
              ),
            ),
            OutlinedButton(onPressed: () => _joinById(roomCtrl.text.trim()), child: const Text('Join by ID')),
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
  String? trump, lead;
  bool handOver = false;
  List<dynamic> hand = [];
  List<Map<String, dynamic>> trick = [];
  List<String> names = [];

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
              lead = m['m']['lead'] as String?;
              handOver = (m['m']['handOver'] ?? false) as bool;
              hand = List<dynamic>.from(m['m']['you'] as List? ?? const []);
              trick = ((m['m']['trick'] as List?) ?? const [])
                  .map((e) => Map<String, dynamic>.from((e as Map).map((k, v) => MapEntry(k.toString(), v))))
                  .toList();
              names = ((m['m']['names'] as List?) ?? const []).map((e) => (e ?? '').toString()).toList();
            });
          }
          break;
        case 'chat':
          if ((m['m']['room'] ?? '') == widget.roomId) {
            final fromName = (m['m']['from_name'] ?? '').toString();
            final from = fromName.isNotEmpty ? fromName : (m['m']['from'] ?? 'player').toString();
            final text = (m['m']['text'] ?? '').toString();
            setState(() => chat.add('[$from] $text'));
          }
          break;
      }
    });
  }

  @override
  void dispose() { chatCtrl.dispose(); super.dispose(); }

  void _sendChat() {
    final t = chatCtrl.text.trim();
    if (t.isEmpty) return;
    widget.ws.send({"t":"chat","m":{"room": widget.roomId, "text": t}});
    chatCtrl.clear();
  }

  void _leave() {
    widget.ws.send({"t":"leave_table","m":{"room": widget.roomId}});
    Navigator.of(context).pop();
  }

  void _newHand() {
    widget.ws.send({"t":"new_hand","m":{"room": widget.roomId}});
  }

  @override
  Widget build(BuildContext context) {
    final myTurn = seat != null && turn != null && seat == turn;
    return Scaffold(
      appBar: AppBar(
        title: Text('Table — ${widget.roomId}'),
        leading: IconButton(icon: const Icon(Icons.arrow_back), onPressed: _leave),
        actions: [
          if (handOver) Padding(
            padding: const EdgeInsets.symmetric(horizontal: 8),
            child: FilledButton(onPressed: _newHand, child: const Text('New hand')),
          ),
        ],
      ),
      body: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Seat: ${seat ?? "-"}  |  Turn: ${turn ?? "-"}  |  Trump: ${trump ?? "-"}  |  Lead: ${lead ?? "-"}'),
            const SizedBox(height: 4),
            if (names.isNotEmpty)
              Text('Players: ${names.asMap().entries.map((e) => "s${e.key}:${e.value.isEmpty ? '—' : e.value}").join("  ")}'),
            const Divider(),
            const Text('On table (current trick):'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: trick.map<Widget>((t) {
                final rank = t['rank']?.toString() ?? '?';
                final suit = t['suit']?.toString() ?? '?';
                final by = (t['by'] as num?)?.toInt();
                return Chip(label: Text('$rank-$suit  (s${by ?? "?"})'));
              }).toList(),
            ),
            const Divider(),
            const Text('Your hand:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: hand.map<Widget>((c) {
                final suit = c['Suit'] as String? ?? '?';
                final rank = c['Rank'] as String? ?? '?';
                return OutlinedButton(
                  onPressed: (myTurn && !handOver) ? () {
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
