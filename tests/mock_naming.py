"""Handle automatic session naming separately from scripted agent responses."""
import json


def respond_to_naming(handler, body):
    if not body.get('instructions', '').startswith('Name this coding session'):
        return False
    title = 'Fixture Research Session'
    events = [
        {'type': 'response.output_text.delta', 'delta': title},
        {'type': 'response.output_item.done', 'output_index': 0, 'item': {
            'type': 'message', 'id': 'naming-message', 'role': 'assistant',
            'status': 'completed', 'content': [
                {'type': 'output_text', 'text': title, 'annotations': []}]}},
        {'type': 'response.completed', 'response': {'id': 'naming-response',
            'usage': {'input_tokens': 10, 'output_tokens': 4}}},
    ]
    handler.send_response(200)
    handler.send_header('Content-Type', 'text/event-stream')
    handler.end_headers()
    for event in events:
        handler.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
    return True
